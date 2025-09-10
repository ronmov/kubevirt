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

package generic

import (
	"fmt"
	"os"
	"path/filepath"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	drautil "kubevirt.io/kubevirt/pkg/dra"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/device"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/device/hostdevice"
)

const (
	failedCreateGenericHostDevicesFmt = "failed to create generic host-devices: %v"
	AliasPrefix                       = "hostdevice-"
	DefaultDisplayOff                 = false
	pciBasePath                       = "/sys/bus/pci/devices"
)

func CreateHostDevices(vmiHostDevices []v1.HostDevice) ([]api.HostDevice, error) {
	return CreateHostDevicesFromPools(vmiHostDevices,
		NewPCIAddressPool(vmiHostDevices), NewMDEVAddressPool(vmiHostDevices), NewUSBAddressPool(vmiHostDevices))
}

func CreateHostDevicesFromPools(vmiHostDevices []v1.HostDevice, pciAddressPool, mdevAddressPool, usbAddressPool hostdevice.AddressPooler) ([]api.HostDevice, error) {
	pciPool := hostdevice.NewBestEffortAddressPool(pciAddressPool)
	mdevPool := hostdevice.NewBestEffortAddressPool(mdevAddressPool)
	usbPool := hostdevice.NewBestEffortAddressPool(usbAddressPool)

	hostDevicesMetaData := createHostDevicesMetadata(vmiHostDevices)
	pciHostDevices, err := hostdevice.CreatePCIHostDevices(hostDevicesMetaData, pciPool)
	if err != nil {
		return nil, fmt.Errorf(failedCreateGenericHostDevicesFmt, err)
	}
	mdevHostDevices, err := hostdevice.CreateMDEVHostDevices(hostDevicesMetaData, mdevPool, DefaultDisplayOff)
	if err != nil {
		return nil, fmt.Errorf(failedCreateGenericHostDevicesFmt, err)
	}

	hostDevices := append(pciHostDevices, mdevHostDevices...)

	usbHostDevices, err := hostdevice.CreateUSBHostDevices(hostDevicesMetaData, usbPool)
	if err != nil {
		return nil, err
	}

	hostDevices = append(hostDevices, usbHostDevices...)

	if err := validateCreationOfDevicePluginsDevices(vmiHostDevices, hostDevices); err != nil {
		return nil, fmt.Errorf(failedCreateGenericHostDevicesFmt, err)
	}

	return hostDevices, nil
}

func createHostDevicesMetadata(vmiHostDevices []v1.HostDevice) []hostdevice.HostDeviceMetaData {
	var hostDevicesMetaData []hostdevice.HostDeviceMetaData
	for _, dev := range vmiHostDevices {
		hostDevicesMetaData = append(hostDevicesMetaData, hostdevice.HostDeviceMetaData{
			AliasPrefix:  AliasPrefix,
			Name:         dev.Name,
			ResourceName: dev.DeviceName,
		})
	}
	return hostDevicesMetaData
}

// validateCreationOfDevicePluginsDevices validates that all specified generic host-devices have a matching host-device.
// On validation failure, an error is returned.
// The validation assumes that the assignment of a device to a specified generic host-device is correct,
// therefore a simple quantity check is sufficient.
func validateCreationOfDevicePluginsDevices(genericHostDevices []v1.HostDevice, hostDevices []api.HostDevice) error {
	hostDevsWithDP := []v1.HostDevice{}
	for _, hd := range genericHostDevices {
		if !drautil.IsHostDeviceDRA(hd) {
			hostDevsWithDP = append(hostDevsWithDP, hd)
		}
	}

	if len(hostDevsWithDP) > 0 && len(hostDevsWithDP) != len(hostDevices) {
		return fmt.Errorf("the number of device plugin HostDevice/s do not match the number of devices:\nHostDevice: %v\nDevice: %v", hostDevsWithDP, hostDevices)
	}
	return nil
}

func getDeviceIdentifier(addr *api.Address) string {
	return addr.Domain + addr.Bus + addr.Slot
}

func getPCIDeviceToFunctions() (map[string][]string, error) {
	pciDeviceToFunctions := make(map[string][]string)
	err := filepath.Walk(pciBasePath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		hostAddr, err := device.NewPciAddressField(info.Name())
		if err != nil {
			return nil
		}

		deviceID := getDeviceIdentifier(hostAddr)
		pciDeviceToFunctions[deviceID] = append(pciDeviceToFunctions[deviceID], hostAddr.Function)
		return nil
	})
	if err != nil {
		log.DefaultLogger().Reason(err).Errorf("failed to discover PCI device functions")
		return nil, err
	}
	return pciDeviceToFunctions, nil
}

// Constructs a list of HostDevices for a given set of VMI HostDevice definitions,
// specifically handling multifunction PCI devices.
//
// For each multifunction PCI device, this function guarantees that all associated functions of the same device
// will appear in order in the resulting slice. The primary function is listed first, followed by any additional
// functions discovered via getPCIDeviceToFunctions.
func CreateMultiFunctionHostDevices(vmiHostDevices []v1.HostDevice) ([]api.HostDevice, error) {
	var hostDevices []api.HostDevice

	multiFunctionPCIPool := hostdevice.NewBestEffortAddressPool(NewMultiFunctionPCIAddressPool(vmiHostDevices))
	hostDevicesMetaData := createHostDevicesMetadata(vmiHostDevices)

	PCIDeviceToFunctions, err := getPCIDeviceToFunctions()
	if err != nil {
		log.DefaultLogger().Reason(err).Error("failed to get PCI device functions")
		return nil, err
	}

	for _, deviceMetaData := range hostDevicesMetaData {
		address, err := multiFunctionPCIPool.Pop(deviceMetaData.ResourceName)
		if err != nil {
			return nil, fmt.Errorf(hostdevice.FailedCreateHostDeviceFmt, deviceMetaData.Name, err)
		}
		if address == "" {
			continue // Not a multifunction PCI device
		}

		hostDevice, err := hostdevice.CreatePCIHostDevice(deviceMetaData, address)
		if err != nil {
			return nil, fmt.Errorf(hostdevice.FailedCreateHostDeviceFmt, deviceMetaData.Name, err)
		}

		hostDevices = append(hostDevices, *hostDevice)

		deviceID := getDeviceIdentifier(hostDevice.Source.Address)
		for _, currHostFunction := range PCIDeviceToFunctions[deviceID] {
			currDeviceCopy := hostDevice.DeepCopy()
			currDeviceCopy.Source.Address.Function = currHostFunction
			currDeviceCopy.Alias = api.NewUserDefinedAlias(currDeviceCopy.Alias.GetName() + "-func-" + currHostFunction)
			hostDevices = append(hostDevices, *currDeviceCopy)
		}
	}
	return hostDevices, nil
}
