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
	"strconv"
	"strings"
	"sync"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/kubevirt/pkg/util"
	pluginapi "kubevirt.io/kubevirt/pkg/virt-handler/device-manager/deviceplugin/v1beta1"
)

type MultiFunctionPCIDevicePlugin struct {
	*PCIDevicePluginBase
	numberOfFunctions                          int
	function0IommuGroupToAssociatedIommuGroups map[string][]string
}

func (dpi *MultiFunctionPCIDevicePlugin) Allocate(_ context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	var allocatedDevices []string
	resp := new(pluginapi.AllocateResponse)
	containerResponse := new(pluginapi.ContainerAllocateResponse)
	resourceNameEnvVar := util.ResourceNameToEnvVar(v1.MultiFunctionPCIResourcePrefix, dpi.resourceName)

	for _, request := range r.ContainerRequests {
		deviceSpecs := make([]*pluginapi.DeviceSpec, 0)
		for _, devID := range request.DevicesIDs {
			// translate device's iommu group to its pci address
			devPCIAddress, exist := dpi.iommuToPCIMap[devID]
			if !exist {
				continue
			}

			// Add VFIO device (iommu group) to the container allocation requirements
			allocatedDevices = append(allocatedDevices, devPCIAddress)
			deviceSpecs = append(deviceSpecs, formatVFIODeviceSpecs(devID)...)

			// Add additional VFIO devices (iommu groups) belonging to the same multi-function device
			// to the container allocation requirements
			for _, iommuGroup := range dpi.function0IommuGroupToAssociatedIommuGroups[devID] {
				deviceSpecs = append(deviceSpecs, formatVFIODeviceSpecs(iommuGroup)...)
			}
		}
		containerResponse.Devices = deviceSpecs
		envVar := make(map[string]string)
		envVar[resourceNameEnvVar] = strings.Join(allocatedDevices, ",")

		// Pass virt-launcher the count of functions that are associated with this multi-function device
		envVar[util.ResourceNameToEnvVar(v1.MultiFunctionCountPCIResourcePrefix, dpi.resourceName)] = strconv.FormatInt(int64(dpi.numberOfFunctions), 10)

		containerResponse.Envs = envVar
		resp.ContainerResponses = append(resp.ContainerResponses, containerResponse)
	}
	return resp, nil
}

func NewMultiFunctionPCIDevicePlugin(resources MultiFunctionPciResourceDescriptor, resourceName string) *MultiFunctionPCIDevicePlugin {
	serverSock := createServerSockerPath(resourceName)
	devs, iommuToPCIMap, function0IommuGroupToAssociatedIommuGroups := constructMultiFunctionDPIdevices(resources.devices)

	dpi := &MultiFunctionPCIDevicePlugin{
		PCIDevicePluginBase: &PCIDevicePluginBase{
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
			iommuToPCIMap: iommuToPCIMap,
		},
		function0IommuGroupToAssociatedIommuGroups: function0IommuGroupToAssociatedIommuGroups,
		numberOfFunctions:                          resources.NumberOfFunctions,
	}
	return dpi
}

func constructMultiFunctionDPIdevices(pciDevices []*MultiFunctionPCIDevice) ([]*pluginapi.Device, map[string]string, map[string][]string) {
	var devs []*pluginapi.Device
	var iommuToPCIMap = map[string]string{}
	var function0IommuGroupToAssociatedIommuGroups = map[string][]string{}

	for _, pciDevice := range pciDevices {
		iommuToPCIMap[pciDevice.function0.iommuGroup] = pciDevice.function0.pciAddress
		for _, associatedPciDevice := range pciDevice.associatedFunctions {
			function0IommuGroupToAssociatedIommuGroups[pciDevice.function0.iommuGroup] = append(function0IommuGroupToAssociatedIommuGroups[pciDevice.function0.iommuGroup], associatedPciDevice.iommuGroup)
		}

		dpiDev := constructDPIdevice(pciDevice.function0)
		devs = append(devs, dpiDev)
	}
	return devs, iommuToPCIMap, function0IommuGroupToAssociatedIommuGroups
}
