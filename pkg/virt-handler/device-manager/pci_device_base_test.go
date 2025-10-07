package device_manager

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	v1 "kubevirt.io/api/core/v1"

	"k8s.io/apimachinery/pkg/util/yaml"

	"kubevirt.io/kubevirt/pkg/testutils"
	"kubevirt.io/kubevirt/pkg/virt-config/featuregate"
)

const (
	singleFunctionFakeName     = "example.org/deadbeef"
	multiFunctionFakeName      = "example.org/deadbeef_2F"
	singleFunctionFakeID       = "dead:beef"
	multiFunctionFakeID        = "bead:feeb"
	fakeDriver                 = "vfio-pci"
	singleFunctionFakeAddress  = "0000:00:00.0"
	multiFunction1FakeAddress0 = "0000:01:00.0"
	multiFunction1FakeAddress1 = "0000:01:00.1"
	multiFunction2FakeAddress0 = "0000:02:00.0"
	multiFunction2FakeAddress2 = "0000:02:00.2" // intentionally have a "hole" at function 1 of this device
	generalFakeAddress         = "0000:03:00.0"

	singleFunctionFakeIommuGroup          = "0"
	multiFunction1Function0FakeIommuGroup = "1" // simulate both functions of multiFunction1 on the same iommu group
	multiFunction1Function1FakeIommuGroup = "1"
	multiFunction2Function0FakeIommuGroup = "2" // simulate functions of multiFunction2 on different iommu groups
	multiFunction2Function1FakeIommuGroup = "3"
	fakeNumaNode                          = 0
)

// simple fake implementation of os.FileInfo
type fakeInfo struct {
	name string
}

func (f fakeInfo) Name() string           { return f.name }
func (f fakeInfo) Size() int64            { return 0 }           // not used, keep zero value
func (f fakeInfo) Mode() os.FileMode      { return 0 }           // not used, keep zero value
func (f fakeInfo) ModTime() (t time.Time) { return time.Time{} } // not used, keep zero value
func (f fakeInfo) IsDir() bool            { return false }       // files in "/sys/bus/pci/devices" are actually links to directories
func (f fakeInfo) Sys() interface{}       { return nil }

func fakePciTreeWalk(root string, fn filepath.WalkFunc) error {
	const expectedPath = "/sys/bus/pci/devices"
	mockFiles := []fakeInfo{
		{singleFunctionFakeAddress},
		{multiFunction1FakeAddress0},
		{multiFunction1FakeAddress1},
		{multiFunction2FakeAddress0},
		{multiFunction2FakeAddress2},
		{generalFakeAddress},
	}
	if root != expectedPath {
		return fmt.Errorf("expected implementation to call MockableWalk on %s", expectedPath)
	}
	for _, file := range mockFiles {
		if err := fn(path.Join(root, file.name), file, nil); err != nil {
			return err
		}
	}
	return nil
}

var _ = Describe("single-function PCI Device", func() {
	var mockPCI *MockDeviceHandler
	var fakePermittedHostDevicesConfig string
	var fakePermittedHostDevices v1.PermittedHostDevices
	var ctrl *gomock.Controller
	var clientTest *fake.Clientset

	// restore original after test
	origWalk := MockableWalk
	AfterEach(func() {
		MockableWalk = origWalk
	})

	BeforeEach(func() {
		MockableWalk = fakePciTreeWalk
		clientTest = fake.NewSimpleClientset()

		By("mocking PCI functions to simulate a vfio-pci device at " + singleFunctionFakeAddress)
		ctrl = gomock.NewController(GinkgoT())
		mockPCI = NewMockDeviceHandler(ctrl)
		handler = mockPCI
		// Force pre-defined returned values and ensure the function only get called exactly once each on singleFunctionFakeAddress
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, singleFunctionFakeAddress).Return(singleFunctionFakeIommuGroup, nil).Times(1)
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, singleFunctionFakeAddress).Return(fakeDriver, nil).Times(1)
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, singleFunctionFakeAddress).Return(fakeNumaNode).Times(1)
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, singleFunctionFakeAddress).Return(singleFunctionFakeID, nil).Times(1)
		// Allow the regular functions to be called for all the other devices, they're harmless.
		// Just force the driver to NOT vfio-pci to ensure they all get ignored.
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, gomock.Any()).Return("definitely-not-vfio-pci", nil).AnyTimes()
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().IsDeviceVirtualFunction(pciBasePath, gomock.Any()).Times(0)

		By("creating a list of fake device using the yaml decoder")
		fakePermittedHostDevicesConfig = `
pciHostDevices:
- pciVendorSelector: "` + singleFunctionFakeID + `"
  resourceName: "` + singleFunctionFakeName + `"
`
		err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(fakePermittedHostDevicesConfig), 1024).Decode(&fakePermittedHostDevices)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakePermittedHostDevices.PciHostDevices).To(HaveLen(1))
		Expect(fakePermittedHostDevices.PciHostDevices[0].PCIVendorSelector).To(Equal(singleFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[0].ResourceName).To(Equal(singleFunctionFakeName))
	})

	It("Should parse the permitted devices and find 1 matching PCI device", func() {
		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices, true)
		Expect(err).ToNot(HaveOccurred())
		devices := discoverPermittedHostPCIDevices(supportedPCIDeviceMap)
		Expect(devices).To(HaveLen(1), "only one single-function PCI device is expected to be found")
		Expect(devices[singleFunctionFakeName].devices).To(HaveLen(1), "only one single-function PCI device is expected to be found")
		Expect(devices[singleFunctionFakeName].devices[0].pciID).To(Equal(singleFunctionFakeID))
		Expect(devices[singleFunctionFakeName].devices[0].driver).To(Equal(fakeDriver))
		Expect(devices[singleFunctionFakeName].devices[0].pciAddress).To(Equal(singleFunctionFakeAddress))
		Expect(devices[singleFunctionFakeName].devices[0].iommuGroup).To(Equal(singleFunctionFakeIommuGroup))
		Expect(devices[singleFunctionFakeName].devices[0].numaNode).To(Equal(fakeNumaNode))
	})

	It("Should validate DPI devices", func() {
		iommuToPCIMap := make(map[string]string)
		multifunctionAdditionalIommuGroups := make(map[string][]string)
		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices, true)
		Expect(err).ToNot(HaveOccurred())
		pciDevices := discoverPermittedHostPCIDevices(supportedPCIDeviceMap)
		devs := constructDPIdevices(pciDevices[singleFunctionFakeName].devices, iommuToPCIMap, multifunctionAdditionalIommuGroups)
		Expect(devs[0].ID).To(Equal(singleFunctionFakeIommuGroup))
		Expect(devs[0].Topology.Nodes[0].ID).To(Equal(int64(fakeNumaNode)))
	})
	It("Should update the device list according to the configmap", func() {
		By("creating a cluster config")
		kv := &v1.KubeVirt{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kubevirt",
				Namespace: "kubevirt",
			},
			Spec: v1.KubeVirtSpec{
				Configuration: v1.KubeVirtConfiguration{
					DeveloperConfiguration: &v1.DeveloperConfiguration{},
				},
			},
			Status: v1.KubeVirtStatus{
				Phase: v1.KubeVirtPhaseDeploying,
			},
		}
		fakeClusterConfig, _, kvStore := testutils.NewFakeClusterConfigUsingKV(kv)

		By("creating an empty device controller")
		var noDevices []Device
		deviceController := NewDeviceController("master", 100, "rw", noDevices, fakeClusterConfig, clientTest.CoreV1())

		By("adding a host device to the cluster config")
		kvConfig := kv.DeepCopy()
		kvConfig.Spec.Configuration.DeveloperConfiguration.FeatureGates = []string{featuregate.HostDevicesGate}
		kvConfig.Spec.Configuration.PermittedHostDevices = &v1.PermittedHostDevices{
			PciHostDevices: []v1.PciHostDevice{
				{
					PCIVendorSelector: singleFunctionFakeID,
					ResourceName:      singleFunctionFakeName,
				},
			},
		}
		testutils.UpdateFakeKubeVirtClusterConfig(kvStore, kvConfig)
		permittedDevices := fakeClusterConfig.GetPermittedHostDevices()
		Expect(permittedDevices).ToNot(BeNil(), "something went wrong while parsing the configmap(s)")
		Expect(permittedDevices.PciHostDevices).To(HaveLen(1), "the fake device was not found")

		By("ensuring a device plugin gets created for our fake device")
		enabledDevicePlugins, disabledDevicePlugins := deviceController.splitPermittedDevices(
			deviceController.updatePermittedHostDevicePlugins(),
		)
		Expect(enabledDevicePlugins).To(HaveLen(1), "a device plugin wasn't created for the fake device")
		Expect(disabledDevicePlugins).To(BeEmpty(), "no disabled device plugins are expected")
		Ω(enabledDevicePlugins).Should(HaveKey(singleFunctionFakeName))
		// Manually adding the enabled plugin, since the device controller is not actually running
		deviceController.startedPlugins[singleFunctionFakeName] = controlledDevice{devicePlugin: enabledDevicePlugins[singleFunctionFakeName]}

		By("deletting the device from the configmap")
		kvConfig.Spec.Configuration.PermittedHostDevices = &v1.PermittedHostDevices{}
		testutils.UpdateFakeKubeVirtClusterConfig(kvStore, kvConfig)
		permittedDevices = fakeClusterConfig.GetPermittedHostDevices()
		Expect(permittedDevices).ToNot(BeNil(), "something went wrong while parsing the configmap(s)")
		Expect(permittedDevices.PciHostDevices).To(BeEmpty(), "the fake device was not deleted")

		By("ensuring the device plugin gets stopped")
		enabledDevicePlugins, disabledDevicePlugins = deviceController.splitPermittedDevices(
			deviceController.updatePermittedHostDevicePlugins(),
		)
		Expect(enabledDevicePlugins).To(BeEmpty(), "no enabled device plugins should be found")
		Expect(disabledDevicePlugins).To(HaveLen(1), "the fake device plugin did not get disabled")
		Ω(disabledDevicePlugins).Should(HaveKey(singleFunctionFakeName))
	})
})

var _ = Describe("multi-function PCI Device", func() {
	var mockPCI *MockDeviceHandler
	var fakePermittedHostDevicesConfig string
	var fakePermittedHostDevices v1.PermittedHostDevices
	var ctrl *gomock.Controller

	// restore original after test
	origWalk := MockableWalk
	AfterEach(func() {
		MockableWalk = origWalk
	})

	BeforeEach(func() {
		MockableWalk = fakePciTreeWalk

		By("mocking PCI functions to simulate a vfio-pci device at " + multiFunction1FakeAddress0 + " and " + multiFunction1FakeAddress1)
		ctrl = gomock.NewController(GinkgoT())
		mockPCI = NewMockDeviceHandler(ctrl)
		handler = mockPCI
		// Force pre-defined returned values and ensure the function only get called exacly once each on multiFunction1FakeAddress0 and multiFunction1FakeAddress1
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, multiFunction1FakeAddress0).Return(multiFunction1Function0FakeIommuGroup, nil).Times(1)
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, multiFunction1FakeAddress0).Return(fakeDriver, nil).Times(1)
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, multiFunction1FakeAddress0).Return(fakeNumaNode).Times(1)
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, multiFunction1FakeAddress0).Return(multiFunctionFakeID, nil).Times(1)
		mockPCI.EXPECT().IsDeviceVirtualFunction(pciBasePath, multiFunction1FakeAddress0).Times(0)
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, multiFunction1FakeAddress1).Times(0) // TODO: makes sense?
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, multiFunction1FakeAddress1).Return(fakeDriver, nil).Times(1)
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, multiFunction1FakeAddress1).Times(0)
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, multiFunction1FakeAddress1).Return(multiFunctionFakeID, nil).Times(1)
		mockPCI.EXPECT().IsDeviceVirtualFunction(pciBasePath, multiFunction1FakeAddress1).Return(false, nil).Times(1)
		// Allow the regular functions to be called for all the other devices, they're harmless.
		// Just force the driver to NOT vfio-pci to ensure they all get ignored.
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, gomock.Any()).Return("definitely-not-vfio-pci", nil).AnyTimes()
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().IsDeviceVirtualFunction(pciBasePath, gomock.Any()).Times(0)

		By("creating a list of fake device using the yaml decoder")
		fakePermittedHostDevicesConfig = `
pciHostDevices:
- pciVendorSelector: "` + multiFunctionFakeID + `"
  resourceName: "` + multiFunctionFakeName + `"
  numberOfFunctions: 2
`
		err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(fakePermittedHostDevicesConfig), 1024).Decode(&fakePermittedHostDevices)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakePermittedHostDevices.PciHostDevices).To(HaveLen(1))
		Expect(fakePermittedHostDevices.PciHostDevices[0].PCIVendorSelector).To(Equal(multiFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[0].ResourceName).To(Equal(multiFunctionFakeName))
	})

	It("Should parse the permitted devices and find 1 matching PCI device", func() {
		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices, true)
		Expect(err).ToNot(HaveOccurred())
		devices := discoverPermittedHostPCIDevices(supportedPCIDeviceMap)
		Expect(devices).To(HaveLen(1), "only one multi-function PCI device is expected to be found")
		Expect(devices[multiFunctionFakeName].devices).To(HaveLen(1), "only one multi-function PCI device is expected to be found")
		Expect(devices[multiFunctionFakeName].devices[0].pciID).To(Equal(multiFunctionFakeID))
		Expect(devices[multiFunctionFakeName].devices[0].driver).To(Equal(fakeDriver))
		Expect(devices[multiFunctionFakeName].devices[0].pciAddress).To(Equal(multiFunction1FakeAddress0))
		Expect(devices[multiFunctionFakeName].devices[0].iommuGroup).To(Equal(multiFunction1Function0FakeIommuGroup))
		Expect(devices[multiFunctionFakeName].devices[0].numaNode).To(Equal(fakeNumaNode))
	})

	It("Should validate DPI devices", func() {
		iommuToPCIMap := make(map[string]string)
		multifunctionAdditionalIommuGroups := make(map[string][]string)
		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices, true)
		Expect(err).ToNot(HaveOccurred())
		devices := discoverPermittedHostPCIDevices(supportedPCIDeviceMap)
		devs := constructDPIdevices(devices[multiFunctionFakeName].devices, iommuToPCIMap, multifunctionAdditionalIommuGroups)
		Expect(devs[0].ID).To(Equal(multiFunction1Function0FakeIommuGroup))
		Expect(devs[0].Topology.Nodes[0].ID).To(Equal(int64(fakeNumaNode)))
	})
})

var _ = Describe("both single-function and multi-function PCI Device", func() {
	var mockPCI *MockDeviceHandler
	var fakePermittedHostDevicesConfig string
	var fakePermittedHostDevices v1.PermittedHostDevices
	var ctrl *gomock.Controller

	// restore original after test
	origWalk := MockableWalk
	AfterEach(func() {
		MockableWalk = origWalk
	})

	BeforeEach(func() {
		MockableWalk = fakePciTreeWalk

		By("mocking PCI functions to simulate a vfio-pci device at " + singleFunctionFakeAddress + ", " + multiFunction1FakeAddress0 + " and " + multiFunction1FakeAddress1)
		ctrl = gomock.NewController(GinkgoT())
		mockPCI = NewMockDeviceHandler(ctrl)
		handler = mockPCI
		// Force pre-defined returned values and ensure the function only get called exacly once each on 0000:00:00.0
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, singleFunctionFakeAddress).Return(singleFunctionFakeIommuGroup, nil).Times(1)
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, singleFunctionFakeAddress).Return(fakeDriver, nil).Times(1)
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, singleFunctionFakeAddress).Return(fakeNumaNode).Times(1)
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, singleFunctionFakeAddress).Return(singleFunctionFakeID, nil).Times(1)
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, multiFunction1FakeAddress0).Return(multiFunction1Function0FakeIommuGroup, nil).Times(1)
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, multiFunction1FakeAddress0).Return(fakeDriver, nil).Times(1)
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, multiFunction1FakeAddress0).Return(fakeNumaNode).Times(1)
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, multiFunction1FakeAddress0).Return(multiFunctionFakeID, nil).Times(1)
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, multiFunction1FakeAddress1).Times(0) // TODO: makes sense?
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, multiFunction1FakeAddress1).Return(fakeDriver, nil).Times(1)
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, multiFunction1FakeAddress1).Times(0)
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, multiFunction1FakeAddress1).Return(multiFunctionFakeID, nil).Times(1)
		mockPCI.EXPECT().IsDeviceVirtualFunction(pciBasePath, gomock.Any()).Return(false, nil).Times(1)
		// Allow the regular functions to be called for all the other devices, they're harmless.
		// Just force the driver to NOT vfio-pci to ensure they all get ignored.
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, gomock.Any()).Return("definitely-not-vfio-pci", nil).AnyTimes()
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().IsDeviceVirtualFunction(pciBasePath, gomock.Any()).Times(0)

		By("creating a list of fake device using the yaml decoder")
		fakePermittedHostDevicesConfig = `
pciHostDevices:
- pciVendorSelector: "` + singleFunctionFakeID + `"
  resourceName: "` + singleFunctionFakeName + `"
- pciVendorSelector: "` + multiFunctionFakeID + `"
  resourceName: "` + multiFunctionFakeName + `"
  numberOfFunctions: 2
`
		err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(fakePermittedHostDevicesConfig), 1024).Decode(&fakePermittedHostDevices)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakePermittedHostDevices.PciHostDevices).To(HaveLen(2))
		Expect(fakePermittedHostDevices.PciHostDevices[0].PCIVendorSelector).To(Equal(singleFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[0].ResourceName).To(Equal(singleFunctionFakeName))
		Expect(fakePermittedHostDevices.PciHostDevices[1].PCIVendorSelector).To(Equal(multiFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[1].ResourceName).To(Equal(multiFunctionFakeName))
	})

	It("Should parse the permitted devices and find 2 matching PCI devices", func() {
		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices, true)
		Expect(err).ToNot(HaveOccurred())
		devices := discoverPermittedHostPCIDevices(supportedPCIDeviceMap)
		Expect(devices).To(HaveLen(2), "one single-function and one multi-function PCI devices are expected to be found")
		Expect(devices[singleFunctionFakeName].devices).To(HaveLen(1), "one single-function PCI device is expected to be found")
		Expect(devices[singleFunctionFakeName].devices[0].pciID).To(Equal(singleFunctionFakeID))
		Expect(devices[singleFunctionFakeName].devices[0].driver).To(Equal(fakeDriver))
		Expect(devices[singleFunctionFakeName].devices[0].pciAddress).To(Equal(singleFunctionFakeAddress))
		Expect(devices[singleFunctionFakeName].devices[0].iommuGroup).To(Equal(singleFunctionFakeIommuGroup))
		Expect(devices[singleFunctionFakeName].devices[0].numaNode).To(Equal(fakeNumaNode))
		Expect(devices[multiFunctionFakeName].devices).To(HaveLen(1), "one multi-function PCI device is expected to be found")
		Expect(devices[multiFunctionFakeName].devices[0].pciID).To(Equal(multiFunctionFakeID))
		Expect(devices[multiFunctionFakeName].devices[0].driver).To(Equal(fakeDriver))
		Expect(devices[multiFunctionFakeName].devices[0].pciAddress).To(Equal(multiFunction1FakeAddress0))
		Expect(devices[multiFunctionFakeName].devices[0].iommuGroup).To(Equal(multiFunction1Function0FakeIommuGroup))
		Expect(devices[multiFunctionFakeName].devices[0].numaNode).To(Equal(fakeNumaNode))
	})

	It("Should validate DPI devices", func() {
		iommuToPCIMap := make(map[string]string)
		multifunctionAdditionalIommuGroups := make(map[string][]string)
		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices, true)
		Expect(err).ToNot(HaveOccurred())
		devices := discoverPermittedHostPCIDevices(supportedPCIDeviceMap)
		devs := constructDPIdevices(devices[multiFunctionFakeName].devices, iommuToPCIMap, multifunctionAdditionalIommuGroups)
		Expect(devs[0].ID).To(Equal(multiFunction1Function0FakeIommuGroup))
		Expect(devs[0].Topology.Nodes[0].ID).To(Equal(int64(fakeNumaNode)))
		devs = constructDPIdevices(devices[singleFunctionFakeName].devices, iommuToPCIMap, multifunctionAdditionalIommuGroups)
		Expect(devs[0].ID).To(Equal(singleFunctionFakeIommuGroup))
		Expect(devs[0].Topology.Nodes[0].ID).To(Equal(int64(fakeNumaNode)))
	})
})

var _ = Describe("config tests", func() {
	It("should fail to validate config when same vendor selector is registering single-function and multi-function devices", func() {
		fakePermittedHostDevicesConfig := `
pciHostDevices:
- pciVendorSelector: "` + singleFunctionFakeID + `"
  resourceName: "` + singleFunctionFakeName + `"
- pciVendorSelector: "` + singleFunctionFakeID + `"
  resourceName: "` + multiFunctionFakeName + `"
  numberOfFunctions: 2
`
		var fakePermittedHostDevices v1.PermittedHostDevices
		err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(fakePermittedHostDevicesConfig), 1024).Decode(&fakePermittedHostDevices)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakePermittedHostDevices.PciHostDevices).To(HaveLen(2))
		Expect(fakePermittedHostDevices.PciHostDevices[0].PCIVendorSelector).To(Equal(singleFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[0].ResourceName).To(Equal(singleFunctionFakeName))
		Expect(fakePermittedHostDevices.PciHostDevices[1].PCIVendorSelector).To(Equal(singleFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[1].ResourceName).To(Equal(multiFunctionFakeName))

		_, err = validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices, true)
		Expect(err).To(HaveOccurred())
	})

	It("should output a single device when 2 identical single-function devices are configured", func() {
		fakePermittedHostDevicesConfig := `
pciHostDevices:
- pciVendorSelector: "` + singleFunctionFakeID + `"
  resourceName: "` + singleFunctionFakeName + `"
- pciVendorSelector: "` + singleFunctionFakeID + `"
  resourceName: "` + singleFunctionFakeName + `"
`
		var fakePermittedHostDevices v1.PermittedHostDevices
		err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(fakePermittedHostDevicesConfig), 1024).Decode(&fakePermittedHostDevices)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakePermittedHostDevices.PciHostDevices).To(HaveLen(2))
		Expect(fakePermittedHostDevices.PciHostDevices[0].PCIVendorSelector).To(Equal(singleFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[0].ResourceName).To(Equal(singleFunctionFakeName))
		Expect(fakePermittedHostDevices.PciHostDevices[1].PCIVendorSelector).To(Equal(singleFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[1].ResourceName).To(Equal(singleFunctionFakeName))

		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices, true)
		Expect(err).ToNot(HaveOccurred())

		Expect(supportedPCIDeviceMap).To(HaveLen(1))
		Expect(supportedPCIDeviceMap[singleFunctionFakeID].pciResourceVariants).To(HaveLen(1))
	})
})
