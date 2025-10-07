/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package device_manager

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	pluginapi "kubevirt.io/kubevirt/pkg/virt-handler/device-manager/deviceplugin/v1beta1"
)

const (
	vfioDevicePath    = "/dev/vfio/"
	vfioMount         = "/dev/vfio/vfio"
	pciBasePath       = "/sys/bus/pci/devices"
	vfioPciDriverName = "vfio-pci"
)

var MockableWalk = filepath.Walk

type PCIDevice struct {
	pciID      string
	driver     string
	pciAddress string
	iommuGroup string
	numaNode   int
}

type PCIDevicePluginBase struct {
	*DevicePluginBase
	iommuToPCIMap map[string]string
}

type PciResourceVariantDiscoveryDescriptor struct {
	ResourceName      string
	NumberOfFunctions int
}

type PciResourceDiscoveryDescriptor struct {
	PCIVendorSelector   string
	pciResourceVariants []PciResourceVariantDiscoveryDescriptor
}

type SingleFunctionPciResourceDescriptor struct {
	devices []*PCIDevice
}

type MultiFunctionPCIDevice struct {
	function0           *PCIDevice
	associatedFunctions []*PCIDevice
}

type MultiFunctionPciResourceDescriptor struct {
	devices           []*MultiFunctionPCIDevice
	NumberOfFunctions int
}

func (dpi *PCIDevicePluginBase) Start(stop <-chan struct{}) (err error) {
	logger := log.DefaultLogger()
	dpi.stop = stop

	err = dpi.cleanup()
	if err != nil {
		return err
	}

	sock, err := net.Listen("unix", dpi.socketPath)
	if err != nil {
		return fmt.Errorf("error creating GRPC server socket: %v", err)
	}

	dpi.server = grpc.NewServer([]grpc.ServerOption{}...)
	defer dpi.stopDevicePlugin()

	pluginapi.RegisterDevicePluginServer(dpi.server, dpi)

	errChan := make(chan error, 2)

	go func() {
		errChan <- dpi.server.Serve(sock)
	}()

	err = waitForGRPCServer(dpi.socketPath, connectionTimeout)
	if err != nil {
		return fmt.Errorf("error starting the GRPC server: %v", err)
	}

	err = dpi.register()
	if err != nil {
		return fmt.Errorf("error registering with device plugin manager: %v", err)
	}

	go func() {
		errChan <- dpi.healthCheck()
	}()

	dpi.setInitialized(true)
	logger.Infof("%s device plugin started", dpi.resourceName)
	err = <-errChan

	return err
}

func (dpi *PCIDevicePluginBase) healthCheck() error {
	logger := log.DefaultLogger()
	monitoredDevices := make(map[string]string)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to creating a fsnotify watcher: %v", err)
	}
	defer watcher.Close()

	// This way we don't have to mount /dev from the node
	devicePath := filepath.Join(dpi.deviceRoot, dpi.devicePath)

	// Start watching the files before we check for their existence to avoid races
	dirName := filepath.Dir(devicePath)
	err = watcher.Add(dirName)
	if err != nil {
		return fmt.Errorf("failed to add the device root path to the watcher: %v", err)
	}

	_, err = os.Stat(devicePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("could not stat the device: %v", err)
		}
	}

	// probe all devices
	for _, dev := range dpi.devs {
		vfioDevice := filepath.Join(devicePath, dev.ID)
		err = watcher.Add(vfioDevice)
		if err != nil {
			return fmt.Errorf("failed to add the device %s to the watcher: %v", vfioDevice, err)
		}
		monitoredDevices[vfioDevice] = dev.ID
	}

	dirName = filepath.Dir(dpi.socketPath)
	err = watcher.Add(dirName)

	if err != nil {
		return fmt.Errorf("failed to add the device-plugin kubelet path to the watcher: %v", err)
	}
	_, err = os.Stat(dpi.socketPath)
	if err != nil {
		return fmt.Errorf("failed to stat the device-plugin socket: %v", err)
	}

	for {
		select {
		case <-dpi.stop:
			return nil
		case err := <-watcher.Errors:
			logger.Reason(err).Errorf("error watching devices and device plugin directory")
		case event := <-watcher.Events:
			logger.V(4).Infof("health Event: %v", event)
			if monDevId, exist := monitoredDevices[event.Name]; exist {
				// Health in this case is if the device path actually exists
				if event.Op == fsnotify.Create {
					logger.Infof("monitored device %s appeared", dpi.resourceName)
					dpi.health <- deviceHealth{
						DevId:  monDevId,
						Health: pluginapi.Healthy,
					}
				} else if (event.Op == fsnotify.Remove) || (event.Op == fsnotify.Rename) {
					logger.Infof("monitored device %s disappeared", dpi.resourceName)
					dpi.health <- deviceHealth{
						DevId:  monDevId,
						Health: pluginapi.Unhealthy,
					}
				}
			} else if event.Name == dpi.socketPath && event.Op == fsnotify.Remove {
				logger.Infof("device socket file for device %s was removed, kubelet probably restarted.", dpi.resourceName)
				return nil
			}
		}
	}
}

func createServerSockerPath(resourceName string) string {
	return SocketPath(strings.Replace(resourceName, "/", "-", -1))
}

func constructDPIdevice(pciDevice *PCIDevice) *pluginapi.Device {
	dpiDev := &pluginapi.Device{
		ID:     pciDevice.iommuGroup,
		Health: pluginapi.Healthy,
	}
	if pciDevice.numaNode >= 0 {
		numaInfo := &pluginapi.NUMANode{
			ID: int64(pciDevice.numaNode),
		}
		dpiDev.Topology = &pluginapi.TopologyInfo{
			Nodes: []*pluginapi.NUMANode{numaInfo},
		}
	}
	return dpiDev
}

func validatePciHostDevicesConfiguration(KVConfigHostDevices []v1.PciHostDevice) (map[string]PciResourceDiscoveryDescriptor, error) {
	var devicesSet = make(map[string]struct{})

	const (
		singleFunctionDeviceSetSuffix = "-single"
		multiFunctionDeviceSetSuffix  = "-multi"
	)

	supportedPCIDeviceMap := make(map[string]PciResourceDiscoveryDescriptor)
	for _, pciDev := range KVConfigHostDevices {
		if pciDev.NumberOfFunctions == 0 {
			_, found := devicesSet[pciDev.PCIVendorSelector+multiFunctionDeviceSetSuffix]
			if found {
				return nil, fmt.Errorf("pciHostDevice %s is already defined as multi-function resource", pciDev.PCIVendorSelector)
			}
			devicesSet[pciDev.PCIVendorSelector+singleFunctionDeviceSetSuffix] = struct{}{}
		} else {
			_, found := devicesSet[pciDev.PCIVendorSelector+singleFunctionDeviceSetSuffix]
			if found {
				return nil, fmt.Errorf("pciHostDevice %s is already defined as single-function resource", pciDev.PCIVendorSelector)
			}
			devicesSet[pciDev.PCIVendorSelector+multiFunctionDeviceSetSuffix] = struct{}{}
		}

		log.Log.V(4).Infof("Permitted PCI device in the cluster, ID: %s, resourceName: %s, externalProvider: %tL numberOfFunctions: %d",
			strings.ToLower(pciDev.PCIVendorSelector),
			pciDev.ResourceName,
			pciDev.ExternalResourceProvider,
			pciDev.NumberOfFunctions)
		// do not add a device plugin for this resource if it's being provided via an external device plugin
		if pciDev.ExternalResourceProvider {
			continue
		}
		desc := supportedPCIDeviceMap[strings.ToLower(pciDev.PCIVendorSelector)]
		variant := PciResourceVariantDiscoveryDescriptor{pciDev.ResourceName, pciDev.NumberOfFunctions}
		// do not add a variant that already exists
		exists := false
		for _, v := range desc.pciResourceVariants {
			if v == variant {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		desc.PCIVendorSelector = pciDev.PCIVendorSelector
		desc.pciResourceVariants = append(desc.pciResourceVariants, variant)
		supportedPCIDeviceMap[strings.ToLower(pciDev.PCIVendorSelector)] = desc
	}
	return supportedPCIDeviceMap, nil
}

func isDeviceBoundToVfio(address string) (bool, error) {
	driver, err := handler.GetDeviceDriver(pciBasePath, address)
	if err != nil {
		return false, err
	}
	return driver == vfioPciDriverName, nil
}

func handleSingleFunctionDeviceDiscovery(pciID, address, driver string) (*PCIDevice, error) {
	log.DefaultLogger().Infof("registering device: %s", address)

	iommuGroup, err := handler.GetDeviceIOMMUGroup(pciBasePath, address)
	if err != nil {
		return nil, err
	}

	return &PCIDevice{
		pciID:      pciID,
		pciAddress: address,
		iommuGroup: iommuGroup,
		driver:     driver,
		numaNode:   handler.GetDeviceNumaNode(pciBasePath, address),
	}, nil
}

func handleMultiFunctionDeviceDiscovery(pciID, address, driver string, pciResourceVariants []PciResourceVariantDiscoveryDescriptor) (*MultiFunctionPCIDevice, *PciResourceVariantDiscoveryDescriptor, error) {
	var associatedFunctions []*PCIDevice
	device, err := handleSingleFunctionDeviceDiscovery(pciID, address, driver)
	if err != nil {
		return nil, nil, err
	}

	functionCount := 1
	baseAddress := strings.TrimSuffix(address, ".0")

	err = MockableWalk(pciBasePath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasPrefix(info.Name(), baseAddress) || info.Name() == address {
			return nil
		}

		isDeviceVirtualFunction, err := handler.IsDeviceVirtualFunction(pciBasePath, info.Name())
		if err != nil {
			return err
		}
		if isDeviceVirtualFunction {
			return nil
		}

		isBound, err := isDeviceBoundToVfio(info.Name())
		if err != nil {
			return err
		}
		if !isBound {
			return fmt.Errorf("device %s not bound to vfio-pci", info.Name())
		}

		iommuGroup, err := handler.GetDeviceIOMMUGroup(pciBasePath, info.Name())
		if err != nil {
			return err
		}

		functionCount++
		associatedFunctions = append(associatedFunctions, &PCIDevice{
			pciID:      pciID,
			pciAddress: info.Name(),
			iommuGroup: iommuGroup,
			driver:     driver,
			numaNode:   handler.GetDeviceNumaNode(pciBasePath, info.Name()),
		})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	for _, resourceVariant := range pciResourceVariants {
		if resourceVariant.NumberOfFunctions == functionCount {
			return &MultiFunctionPCIDevice{device, associatedFunctions}, &resourceVariant, nil
		}
	}

	return nil, nil, fmt.Errorf("multi-function device with %d functions found but it is not configured for passthrough", functionCount)
}

func discoverPermittedHostPCIDevices(supportedPCIDeviceMap map[string]PciResourceDiscoveryDescriptor) (map[string]SingleFunctionPciResourceDescriptor, map[string]MultiFunctionPciResourceDescriptor) {
	singleFunctionPciDevicesMap := make(map[string]SingleFunctionPciResourceDescriptor)
	multiFunctionPciDevicesMap := make(map[string]MultiFunctionPciResourceDescriptor)

	err := MockableWalk(pciBasePath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		pciID, err := handler.GetDevicePCIID(pciBasePath, info.Name())
		if err != nil {
			log.DefaultLogger().Reason(err).Errorf("failed to get PCI ID for device: %s", info.Name())
			return nil
		}
		if pciDev, supported := supportedPCIDeviceMap[pciID]; supported {
			driver, err := handler.GetDeviceDriver(pciBasePath, info.Name())
			if err != nil {
				log.DefaultLogger().Reason(err).Errorf("failed to get driver for device: %s", info.Name())
				return nil
			}

			if driver != vfioPciDriverName {
				return nil // skip devices that are not bound to VFIO
			}

			isSingleFunctionDevice := len(pciDev.pciResourceVariants) == 1 && pciDev.pciResourceVariants[0].NumberOfFunctions == 0

			if isSingleFunctionDevice {
				deviceVariant := &pciDev.pciResourceVariants[0]
				device, err := handleSingleFunctionDeviceDiscovery(pciID, info.Name(), driver)
				if err != nil {
					desc := singleFunctionPciDevicesMap[deviceVariant.ResourceName]
					desc.devices = append(desc.devices, device)
				}
			} else if !isSingleFunctionDevice && strings.HasSuffix(info.Name(), ".0") {
				device, deviceVariant, err := handleMultiFunctionDeviceDiscovery(pciID, info.Name(), driver, pciDev.pciResourceVariants)
				if err != nil {
					desc := multiFunctionPciDevicesMap[deviceVariant.ResourceName]
					desc.devices = append(desc.devices, device)
				}
			} else {
				return nil
			}
			if err != nil {
				log.DefaultLogger().Reason(err).Errorf("failed to discover device: %s, error: %v", info.Name(), err)
				return nil
			}
		}
		return nil
	})
	if err != nil {
		log.DefaultLogger().Reason(err).Errorf("failed to discover host devices")
	}
	return singleFunctionPciDevicesMap, multiFunctionPciDevicesMap
}
