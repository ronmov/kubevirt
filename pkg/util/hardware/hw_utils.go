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

package hardware

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

const (
	PCI_ADDRESS_PATTERN = `^([\da-fA-F]{4}):([\da-fA-F]{2}):([\da-fA-F]{2})\.([0-7]{1})$`
	PCI_BASE_PATH       = "/sys/bus/pci/devices"
)

// Parse linux cpuset into an array of ints
// See: http://man7.org/linux/man-pages/man7/cpuset.7.html#FORMATS
func ParseCPUSetLine(cpusetLine string, limit int) (cpusList []int, err error) {
	elements := strings.Split(cpusetLine, ",")
	for _, item := range elements {
		cpuRange := strings.Split(item, "-")
		// provided a range: 1-3
		if len(cpuRange) > 1 {
			start, err := strconv.Atoi(cpuRange[0])
			if err != nil {
				return nil, err
			}
			end, err := strconv.Atoi(cpuRange[1])
			if err != nil {
				return nil, err
			}
			// Add cpus to the list. Assuming it's a valid range.
			for cpuNum := start; cpuNum <= end; cpuNum++ {
				if cpusList, err = safeAppend(cpusList, cpuNum, limit); err != nil {
					return nil, err
				}
			}
		} else {
			cpuNum, err := strconv.Atoi(cpuRange[0])
			if err != nil {
				return nil, err
			}
			if cpusList, err = safeAppend(cpusList, cpuNum, limit); err != nil {
				return nil, err
			}
		}
	}
	return
}

func safeAppend(cpusList []int, cpu int, limit int) ([]int, error) {
	if len(cpusList) > limit {
		return nil, fmt.Errorf("rejecting expanding CPU array for safety reasons, limit is %v", limit)
	}
	return append(cpusList, cpu), nil
}

// GetNumberOfVCPUs returns number of vCPUs
// It counts sockets*cores*threads
func GetNumberOfVCPUs(cpuSpec *v1.CPU) int64 {
	vCPUs := cpuSpec.Cores
	if cpuSpec.Sockets != 0 {
		if vCPUs == 0 {
			vCPUs = cpuSpec.Sockets
		} else {
			vCPUs *= cpuSpec.Sockets
		}
	}
	if cpuSpec.Threads != 0 {
		if vCPUs == 0 {
			vCPUs = cpuSpec.Threads
		} else {
			vCPUs *= cpuSpec.Threads
		}
	}
	return int64(vCPUs)
}

type PciAddress struct {
	Domain   string
	Bus      string
	Device   string
	Function string
}

func ParsePciAddress(pciAddress string) (PciAddress, error) {
	pciAddrRegx, err := regexp.Compile(PCI_ADDRESS_PATTERN)
	if err != nil {
		return PciAddress{}, fmt.Errorf("failed to compile pci address pattern, %v", err)
	}
	res := pciAddrRegx.FindStringSubmatch(pciAddress)
	if len(res) == 0 {
		return PciAddress{}, fmt.Errorf("failed to parse pci address %s", pciAddress)
	}
	return PciAddress{res[1], res[2], res[3], res[4]}, nil
}

func GetDeviceNumaNode(pciAddress string) (*uint32, error) {
	pciBasePath := "/sys/bus/pci/devices"
	numaNodePath := filepath.Join(pciBasePath, pciAddress, "numa_node")
	// #nosec No risk for path injection. Reading static path of NUMA node info
	numaNodeStr, err := os.ReadFile(numaNodePath)
	if err != nil {
		return nil, err
	}
	numaNodeStr = bytes.TrimSpace(numaNodeStr)
	numaNodeInt, err := strconv.Atoi(string(numaNodeStr))
	if err != nil {
		return nil, err
	}
	numaNode := uint32(numaNodeInt)
	return &numaNode, nil
}

func GetDeviceAlignedCPUs(pciAddress string) ([]int, error) {
	numaNode, err := GetDeviceNumaNode(pciAddress)
	if err != nil {
		return nil, err
	}
	cpuList, err := GetNumaNodeCPUList(int(*numaNode))
	if err != nil {
		return nil, err
	}
	return cpuList, err
}

func GetNumaNodeCPUList(numaNode int) ([]int, error) {
	filePath := fmt.Sprintf("/sys/bus/node/devices/node%d/cpulist", numaNode)
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	content = bytes.TrimSpace(content)
	cpusList, err := ParseCPUSetLine(string(content[:]), 50000)
	if err != nil {
		return nil, fmt.Errorf("failed to parse cpulist file: %v", err)
	}

	return cpusList, nil
}

func LookupDeviceVCPUAffinity(pciAddress string, domainSpec *api.DomainSpec) ([]uint32, error) {
	alignedVCPUList := []uint32{}
	p2vCPUMap := make(map[string]uint32)
	alignedPhysicalCPUs, err := GetDeviceAlignedCPUs(pciAddress)
	if err != nil {
		return nil, err
	}

	// make sure that the VMI has cpus from this numa node.
	cpuTune := domainSpec.CPUTune.VCPUPin
	for _, vcpuPin := range cpuTune {
		p2vCPUMap[vcpuPin.CPUSet] = vcpuPin.VCPU
	}

	for _, pcpu := range alignedPhysicalCPUs {
		if vCPU, exist := p2vCPUMap[strconv.Itoa(int(pcpu))]; exist {
			alignedVCPUList = append(alignedVCPUList, uint32(vCPU))
		}
	}
	return alignedVCPUList, nil
}

func GetDeviceIdentifier(addr PciAddress) string {
	return addr.Domain + addr.Bus + addr.Device
}

type GetPCIDeviceToFunctionsType func() (map[string][]string, error)

func GetPCIDeviceToFunctions() (map[string][]string, error) {
	pciDeviceToFunctions := make(map[string][]string)
	err := filepath.Walk(PCI_BASE_PATH, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		pciAddress, err := ParsePciAddress(info.Name())
		if err != nil {
			return nil
		}

		// Check if the device is a Virtual Function (VF).
		// A VF's directory contains a 'physfn' symlink.
		physfnPath := filepath.Join(path, "physfn")
		if _, err := os.Stat(physfnPath); err == nil {
			// This device is a VF, so we skip it.
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			// A stat error other than "file not found" is a real issue.
			return err
		}

		deviceID := GetDeviceIdentifier(pciAddress)
		pciDeviceToFunctions[deviceID] = append(pciDeviceToFunctions[deviceID], pciAddress.Function)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to discover PCI device functions")
	}
	return pciDeviceToFunctions, nil
}
