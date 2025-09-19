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

	v1 "kubevirt.io/api/core/v1"

	drautil "kubevirt.io/kubevirt/pkg/dra"
	hwutil "kubevirt.io/kubevirt/pkg/util/hardware"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/device/hostdevice"
)

const (
	failedCreateGenericHostDevicesFmt = "failed to create generic host-devices: %v"
	AliasPrefix                       = "hostdevice-"
	DefaultDisplayOff                 = false
)

func CreateHostDevices(vmiHostDevices []v1.HostDevice) ([]api.HostDevice, error) {
	return CreateHostDevicesFromPools(vmiHostDevices,
		NewPCIAddressPool(vmiHostDevices), NewMultiFunctionPCIAddressPool(vmiHostDevices), NewMDEVAddressPool(vmiHostDevices), NewUSBAddressPool(vmiHostDevices), hwutil.GetPCIDeviceToFunctions)
}

func CreateHostDevicesFromPools(vmiHostDevices []v1.HostDevice, pciAddressPool, multiFunctionPciAddressPool, mdevAddressPool, usbAddressPool hostdevice.AddressPooler, getPCIDeviceToFunctions hwutil.GetPCIDeviceToFunctionsType) ([]api.HostDevice, error) {
	pciPool := hostdevice.NewBestEffortAddressPool(pciAddressPool)
	multiFunctionPciPool := hostdevice.NewBestEffortAddressPool(multiFunctionPciAddressPool)
	mdevPool := hostdevice.NewBestEffortAddressPool(mdevAddressPool)
	usbPool := hostdevice.NewBestEffortAddressPool(usbAddressPool)

	hostDevicesMetaData := createHostDevicesMetadata(vmiHostDevices)
	pciHostDevices, err := hostdevice.CreatePCIHostDevices(hostDevicesMetaData, pciPool)
	if err != nil {
		return nil, fmt.Errorf(failedCreateGenericHostDevicesFmt, err)
	}

	multiFunctionPciHostDevices, err := hostdevice.CreateMultiFunctionPCIHostDevices(hostDevicesMetaData, multiFunctionPciPool, getPCIDeviceToFunctions)
	if err != nil {
		return nil, fmt.Errorf(failedCreateGenericHostDevicesFmt, err)
	}
	hostDevices := append(pciHostDevices, multiFunctionPciHostDevices...)

	mdevHostDevices, err := hostdevice.CreateMDEVHostDevices(hostDevicesMetaData, mdevPool, DefaultDisplayOff)
	if err != nil {
		return nil, fmt.Errorf(failedCreateGenericHostDevicesFmt, err)
	}

	hostDevices = append(hostDevices, mdevHostDevices...)

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
		resourceName := dev.DeviceName
		if dev.DesiredFunctionCount != 0 {
			resourceName = fmt.Sprintf("%s_%dF", resourceName, dev.DesiredFunctionCount)
		}
		hostDevicesMetaData = append(hostDevicesMetaData, hostdevice.HostDeviceMetaData{
			AliasPrefix:  AliasPrefix,
			Name:         dev.Name,
			ResourceName: resourceName,
		})
	}
	return hostDevicesMetaData
}

// validateCreationOfDevicePluginsDevices validates that all specified generic host-devices have a matching host-device.
// On validation failure, an error is returned.
// The validation assumes that the assignment of a device to a specified generic host-device is correct,
// therefore a simple quantity check is sufficient.
func validateCreationOfDevicePluginsDevices(genericHostDevices []v1.HostDevice, hostDevices []api.HostDevice) error {
	expectedDevicesCount := 0
	for _, hd := range genericHostDevices {
		if drautil.IsHostDeviceDRA(hd) {
			continue
		}
		if hd.DesiredFunctionCount == 0 {
			expectedDevicesCount++
		} else {
			expectedDevicesCount += hd.DesiredFunctionCount
		}
	}

	if expectedDevicesCount > 0 && expectedDevicesCount != len(hostDevices) {
		return fmt.Errorf("the number of device plugin HostDevice/s do not match the number of devices:\nHostDeviceCount: %d\nDevices: %v", expectedDevicesCount, hostDevices)
	}
	return nil
}
