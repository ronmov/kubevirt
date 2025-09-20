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
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/util"
	pluginapi "kubevirt.io/kubevirt/pkg/virt-handler/device-manager/deviceplugin/v1beta1"
)

const (
	vfioDevicePath    = "/dev/vfio/"
	vfioMount         = "/dev/vfio/vfio"
	pciBasePath       = "/sys/bus/pci/devices"
	vfioPciDriverName = "vfio-pci"
)

type PCIDevice struct {
	pciID      string
	driver     string
	pciAddress string
	iommuGroup string
	numaNode   int
}

type PCIDevicePlugin struct {
	*DevicePluginBase
	iommuToPCIMap     map[string]string
	NumberOfFunctions int
}

type PciResourceVariantDiscoveryDescriptor struct {
	ResourceName      string
	NumberOfFunctions int
}

type PciResourceDiscoveryDescriptor struct {
	PCIVendorSelector   string
	pciResourceVariants []PciResourceVariantDiscoveryDescriptor
}

type PciResourceDescriptor struct {
	devices           []*PCIDevice
	NumberOfFunctions int
}

func (dpi *PCIDevicePlugin) Start(stop <-chan struct{}) (err error) {
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

func NewPCIDevicePlugin(resources PciResourceDescriptor, resourceName string) *PCIDevicePlugin {
	serverSock := SocketPath(strings.Replace(resourceName, "/", "-", -1))
	iommuToPCIMap := make(map[string]string)

	devs := constructDPIdevices(resources.devices, iommuToPCIMap)

	dpi := &PCIDevicePlugin{
		DevicePluginBase: &DevicePluginBase{
			devs:         devs,
			initialized:  false,
			lock:         &sync.Mutex{},
			socketPath:   serverSock,
			devicePath:   vfioDevicePath,
			resourceName: resourceName,
			deviceRoot:   util.HostRootMount,
			health:       make(chan deviceHealth),
			done:         make(chan struct{}),
			deregistered: make(chan struct{}),
		},
		iommuToPCIMap:     iommuToPCIMap,
		NumberOfFunctions: resources.NumberOfFunctions,
	}
	return dpi
}

func constructDPIdevices(pciDevices []*PCIDevice, iommuToPCIMap map[string]string) (devs []*pluginapi.Device) {
	for _, pciDevice := range pciDevices {
		iommuToPCIMap[pciDevice.iommuGroup] = pciDevice.pciAddress
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
		devs = append(devs, dpiDev)
	}
	return
}

func (dpi *PCIDevicePlugin) Allocate(_ context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	var resourceNameEnvVar string
	if dpi.NumberOfFunctions != 0 {
		resourceNameEnvVar = util.ResourceNameToEnvVar(v1.MultiFunctionPCIResourcePrefix, dpi.resourceName)
	} else {
		resourceNameEnvVar = util.ResourceNameToEnvVar(v1.PCIResourcePrefix, dpi.resourceName)
	}
	allocatedDevices := []string{}
	resp := new(pluginapi.AllocateResponse)
	containerResponse := new(pluginapi.ContainerAllocateResponse)

	for _, request := range r.ContainerRequests {
		deviceSpecs := make([]*pluginapi.DeviceSpec, 0)
		for _, devID := range request.DevicesIDs {
			// translate device's iommu group to its pci address
			devPCIAddress, exist := dpi.iommuToPCIMap[devID]
			if !exist {
				continue
			}
			// TODO: in case of multi-function add all related vfio devices
			allocatedDevices = append(allocatedDevices, devPCIAddress)
			deviceSpecs = append(deviceSpecs, formatVFIODeviceSpecs(devID)...)
		}
		containerResponse.Devices = deviceSpecs
		envVar := make(map[string]string)
		envVar[resourceNameEnvVar] = strings.Join(allocatedDevices, ",")

		if dpi.NumberOfFunctions != 0 {
			envVar[util.ResourceNameToEnvVar(v1.MultiFunctionCountPCIResourcePrefix, dpi.resourceName)] = strconv.FormatInt(int64(dpi.NumberOfFunctions), 10)
		}

		containerResponse.Envs = envVar
		resp.ContainerResponses = append(resp.ContainerResponses, containerResponse)
	}
	return resp, nil
}

func (dpi *PCIDevicePlugin) healthCheck() error {
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

func handleMultiFunctionDeviceDiscovery(pciID, address, driver string, pciResourceVariants []PciResourceVariantDiscoveryDescriptor) (*PCIDevice, *PciResourceVariantDiscoveryDescriptor, error) {
	device, err := handleSingleFunctionDeviceDiscovery(pciID, address, driver)
	if err != nil {
		return nil, nil, err
	}

	functionCount := 1
	baseAddress := strings.TrimSuffix(address, ".0")

	err = filepath.Walk(pciBasePath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasPrefix(info.Name(), baseAddress) || info.Name() == address {
			return nil
		}

		// skip Virtual Functions (VFs)
		physfnPath := filepath.Join(path, "physfn")
		if _, err := os.Stat(physfnPath); err == nil {
			return nil // VF detected
		} else if !errors.Is(err, os.ErrNotExist) {
			return err // Unexpected stat error
		}

		isBound, err := isDeviceBoundToVfio(info.Name())
		if err != nil {
			return err
		}
		if !isBound {
			return fmt.Errorf("device %s not bound to vfio-pci", info.Name())
		}

		functionCount++
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	for _, resourceVariant := range pciResourceVariants {
		if resourceVariant.NumberOfFunctions == functionCount {
			return device, &resourceVariant, nil
		}
	}

	return nil, nil, fmt.Errorf("multi-function device with %d functions found but it is not configured for passthrough", functionCount)
}

func discoverPermittedHostPCIDevices(supportedPCIDeviceMap map[string]PciResourceDiscoveryDescriptor) map[string]PciResourceDescriptor {
	pciDevicesMap := make(map[string]PciResourceDescriptor)

	err := filepath.Walk(pciBasePath, func(path string, info os.FileInfo, err error) error {
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

			var device *PCIDevice
			deviceVariant := &pciDev.pciResourceVariants[0] // at least one variant per supported PCIID exists in the map
			if isSingleFunctionDevice {
				device, err = handleSingleFunctionDeviceDiscovery(pciID, info.Name(), driver)
			} else if !isSingleFunctionDevice && strings.HasSuffix(info.Name(), ".0") {
				device, deviceVariant, err = handleMultiFunctionDeviceDiscovery(pciID, info.Name(), driver, pciDev.pciResourceVariants)
			} else {
				return nil
			}
			if err != nil {
				log.DefaultLogger().Reason(err).Errorf("failed to discover device: %s, error: %v", info.Name(), err)
				return nil
			}

			desc := pciDevicesMap[deviceVariant.ResourceName]
			desc.devices = append(desc.devices, device)
			desc.NumberOfFunctions = deviceVariant.NumberOfFunctions
			pciDevicesMap[deviceVariant.ResourceName] = desc
		}
		return nil
	})
	if err != nil {
		log.DefaultLogger().Reason(err).Errorf("failed to discover host devices")
	}
	return pciDevicesMap
}
