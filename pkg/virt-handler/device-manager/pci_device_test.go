package device_manager

import (
	"errors"
	"os"
	"strings"

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
	fakeName                  = "example.org/deadbeef"
	multiFunctionFakeName     = "example.org/deadbeef_2F"
	singleFunctionFakeID      = "dead:beef"
	multiFunctionFakeID       = "bead:feeb"
	fakeDriver                = "vfio-pci"
	fakeAddress               = "0000:00:00.0"
	multiFunctionFakeAddress1 = "0000:01:00.0"
	multifunctionFakeAddress2 = "0000:01:00.1"
	fakeIommuGroup            = "0"
	fakeNumaNode              = 0
)

var _ = Describe("single-function PCI Device", func() {
	var mockPCI *MockDeviceHandler
	var fakePermittedHostDevicesConfig string
	var fakePermittedHostDevices v1.PermittedHostDevices
	var ctrl *gomock.Controller
	var clientTest *fake.Clientset

	BeforeEach(func() {
		clientTest = fake.NewSimpleClientset()
		By("making sure the environment has a single-function PCI device at " + fakeAddress)
		_, err := os.Stat("/sys/bus/pci/devices/" + fakeAddress)
		if errors.Is(err, os.ErrNotExist) {
			Skip("No PCI device found at " + fakeAddress + ", can't run single-function PCI tests")
		}

		By("mocking PCI functions to simulate a vfio-pci device at " + fakeAddress)
		ctrl = gomock.NewController(GinkgoT())
		mockPCI = NewMockDeviceHandler(ctrl)
		handler = mockPCI
		// Force pre-defined returned values and ensure the function only get called exacly once each on 0000:00:00.0
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, fakeAddress).Return(fakeIommuGroup, nil).Times(1)
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, fakeAddress).Return(fakeDriver, nil).Times(1)
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, fakeAddress).Return(fakeNumaNode).Times(1)
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, fakeAddress).Return(singleFunctionFakeID, nil).Times(1)
		// Allow the regular functions to be called for all the other devices, they're harmless.
		// Just force the driver to NOT vfio-pci to ensure they all get ignored.
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, gomock.Any()).Return("definitely-not-vfio-pci", nil).AnyTimes()
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, gomock.Any()).AnyTimes()

		By("creating a list of fake device using the yaml decoder")
		fakePermittedHostDevicesConfig = `
pciHostDevices:
- pciVendorSelector: "` + singleFunctionFakeID + `"
  resourceName: "` + fakeName + `"
`
		err = yaml.NewYAMLOrJSONDecoder(strings.NewReader(fakePermittedHostDevicesConfig), 1024).Decode(&fakePermittedHostDevices)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakePermittedHostDevices.PciHostDevices).To(HaveLen(1))
		Expect(fakePermittedHostDevices.PciHostDevices[0].PCIVendorSelector).To(Equal(singleFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[0].ResourceName).To(Equal(fakeName))
	})

	It("Should parse the permitted devices and find 1 matching PCI device", func() {
		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices)
		Expect(err).ToNot(HaveOccurred())
		// discoverPermittedHostPCIDevices() will walk real PCI devices wherever the tests are running
		// It's assumed here that it will find a PCI device at 0000:00:00.0
		devices := discoverPermittedHostPCIDevices(supportedPCIDeviceMap)
		Expect(devices).To(HaveLen(1), "only one single-function PCI device is expected to be found")
		Expect(devices[fakeName].devices).To(HaveLen(1), "only one single-function PCI device is expected to be found")
		Expect(devices[fakeName].devices[0].pciID).To(Equal(singleFunctionFakeID))
		Expect(devices[fakeName].devices[0].driver).To(Equal(fakeDriver))
		Expect(devices[fakeName].devices[0].pciAddress).To(Equal(fakeAddress))
		Expect(devices[fakeName].devices[0].iommuGroup).To(Equal(fakeIommuGroup))
		Expect(devices[fakeName].devices[0].numaNode).To(Equal(fakeNumaNode))
	})

	It("Should validate DPI devices", func() {
		iommuToPCIMap := make(map[string]string)
		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices)
		Expect(err).ToNot(HaveOccurred())
		// discoverPermittedHostPCIDevices() will walk real PCI devices wherever the tests are running
		// It's assumed here that it will find a PCI device at 0000:00:00.0
		pciDevices := discoverPermittedHostPCIDevices(supportedPCIDeviceMap)
		devs := constructDPIdevices(pciDevices[fakeName].devices, iommuToPCIMap)
		Expect(devs[0].ID).To(Equal(fakeIommuGroup))
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
					ResourceName:      fakeName,
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
		Ω(enabledDevicePlugins).Should(HaveKey(fakeName))
		// Manually adding the enabled plugin, since the device controller is not actually running
		deviceController.startedPlugins[fakeName] = controlledDevice{devicePlugin: enabledDevicePlugins[fakeName]}

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
		Ω(disabledDevicePlugins).Should(HaveKey(fakeName))
	})
})

var _ = Describe("multi-function PCI Device", func() {
	var mockPCI *MockDeviceHandler
	var fakePermittedHostDevicesConfig string
	var fakePermittedHostDevices v1.PermittedHostDevices
	var ctrl *gomock.Controller

	BeforeEach(func() {
		By("making sure the environment has a multi-function PCI device at " + multiFunctionFakeAddress1 + " and " + multifunctionFakeAddress2)
		_, err := os.Stat("/sys/bus/pci/devices/" + multiFunctionFakeAddress1)
		if errors.Is(err, os.ErrNotExist) {
			Skip("No PCI device found at " + multiFunctionFakeAddress1 + ", can't run multi-function PCI tests")
		}
		_, err = os.Stat("/sys/bus/pci/devices/" + multifunctionFakeAddress2)
		if errors.Is(err, os.ErrNotExist) {
			Skip("No PCI device found at " + multifunctionFakeAddress2 + ", can't run multi-function PCI tests")
		}

		By("mocking PCI functions to simulate a vfio-pci device at " + multiFunctionFakeAddress1 + " and " + multifunctionFakeAddress2)
		ctrl = gomock.NewController(GinkgoT())
		mockPCI = NewMockDeviceHandler(ctrl)
		handler = mockPCI
		// Force pre-defined returned values and ensure the function only get called exacly once each on 0000:00:00.0
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, multiFunctionFakeAddress1).Return(fakeIommuGroup, nil).Times(1)
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, multiFunctionFakeAddress1).Return(fakeDriver, nil).Times(1)
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, multiFunctionFakeAddress1).Return(fakeNumaNode).Times(1)
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, multiFunctionFakeAddress1).Return(multiFunctionFakeID, nil).Times(1)
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, multifunctionFakeAddress2).Times(0)
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, multifunctionFakeAddress2).Return(fakeDriver, nil).Times(1)
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, multifunctionFakeAddress2).Times(0)
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, multifunctionFakeAddress2).Return(multiFunctionFakeID, nil).Times(1)
		// Allow the regular functions to be called for all the other devices, they're harmless.
		// Just force the driver to NOT vfio-pci to ensure they all get ignored.
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, gomock.Any()).Return("definitely-not-vfio-pci", nil).AnyTimes()
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, gomock.Any()).AnyTimes()

		By("creating a list of fake device using the yaml decoder")
		fakePermittedHostDevicesConfig = `
pciHostDevices:
- pciVendorSelector: "` + multiFunctionFakeID + `"
  resourceName: "` + multiFunctionFakeName + `"
  numberOfFunctions: 2
`
		err = yaml.NewYAMLOrJSONDecoder(strings.NewReader(fakePermittedHostDevicesConfig), 1024).Decode(&fakePermittedHostDevices)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakePermittedHostDevices.PciHostDevices).To(HaveLen(1))
		Expect(fakePermittedHostDevices.PciHostDevices[0].PCIVendorSelector).To(Equal(multiFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[0].ResourceName).To(Equal(multiFunctionFakeName))
	})

	It("Should parse the permitted devices and find 1 matching PCI device", func() {
		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices)
		Expect(err).ToNot(HaveOccurred())
		// discoverPermittedHostPCIDevices() will walk real PCI devices wherever the tests are running
		// It's assumed here that it will find the multi-function PCI device at multifunctionFakeAddress 1 and 2
		devices := discoverPermittedHostPCIDevices(supportedPCIDeviceMap)
		Expect(devices).To(HaveLen(1), "only one multi-function PCI device is expected to be found")
		Expect(devices[multiFunctionFakeName].devices).To(HaveLen(1), "only one multi-function PCI device is expected to be found")
		Expect(devices[multiFunctionFakeName].devices[0].pciID).To(Equal(multiFunctionFakeID))
		Expect(devices[multiFunctionFakeName].devices[0].driver).To(Equal(fakeDriver))
		Expect(devices[multiFunctionFakeName].devices[0].pciAddress).To(Equal(multiFunctionFakeAddress1))
		Expect(devices[multiFunctionFakeName].devices[0].iommuGroup).To(Equal(fakeIommuGroup))
		Expect(devices[multiFunctionFakeName].devices[0].numaNode).To(Equal(fakeNumaNode))
	})

	It("Should validate DPI devices", func() {
		iommuToPCIMap := make(map[string]string)
		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices)
		Expect(err).ToNot(HaveOccurred())
		// discoverPermittedHostPCIDevices() will walk real PCI devices wherever the tests are running
		// It's assumed here that it will find the multi-function PCI device at multifunctionFakeAddress 1 and 2
		devices := discoverPermittedHostPCIDevices(supportedPCIDeviceMap)
		devs := constructDPIdevices(devices[multiFunctionFakeName].devices, iommuToPCIMap)
		Expect(devs[0].ID).To(Equal(fakeIommuGroup))
		Expect(devs[0].Topology.Nodes[0].ID).To(Equal(int64(fakeNumaNode)))
	})
})

var _ = Describe("both single-function and multi-function PCI Device", func() {
	var mockPCI *MockDeviceHandler
	var fakePermittedHostDevicesConfig string
	var fakePermittedHostDevices v1.PermittedHostDevices
	var ctrl *gomock.Controller

	BeforeEach(func() {
		By("making sure the environment has a single-function and a multi-function PCI device at " + fakeAddress + ", " + multiFunctionFakeAddress1 + " and " + multifunctionFakeAddress2)
		_, err := os.Stat("/sys/bus/pci/devices/" + fakeAddress)
		if errors.Is(err, os.ErrNotExist) {
			Skip("No PCI device found at " + fakeAddress + ", can't run single-function and multi-function PCI tests")
		}

		_, err = os.Stat("/sys/bus/pci/devices/" + multiFunctionFakeAddress1)
		if errors.Is(err, os.ErrNotExist) {
			Skip("No PCI device found at " + multiFunctionFakeAddress1 + ", can't run single-function and multi-function PCI tests")
		}
		_, err = os.Stat("/sys/bus/pci/devices/" + multifunctionFakeAddress2)
		if errors.Is(err, os.ErrNotExist) {
			Skip("No PCI device found at " + multifunctionFakeAddress2 + ", can't run single-function and multi-function PCI tests")
		}

		By("mocking PCI functions to simulate a vfio-pci device at " + fakeAddress + ", " + multiFunctionFakeAddress1 + " and " + multifunctionFakeAddress2)
		ctrl = gomock.NewController(GinkgoT())
		mockPCI = NewMockDeviceHandler(ctrl)
		handler = mockPCI
		// Force pre-defined returned values and ensure the function only get called exacly once each on 0000:00:00.0
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, fakeAddress).Return(fakeIommuGroup, nil).Times(1)
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, fakeAddress).Return(fakeDriver, nil).Times(1)
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, fakeAddress).Return(fakeNumaNode).Times(1)
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, fakeAddress).Return(singleFunctionFakeID, nil).Times(1)
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, multiFunctionFakeAddress1).Return(fakeIommuGroup, nil).Times(1)
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, multiFunctionFakeAddress1).Return(fakeDriver, nil).Times(1)
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, multiFunctionFakeAddress1).Return(fakeNumaNode).Times(1)
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, multiFunctionFakeAddress1).Return(multiFunctionFakeID, nil).Times(1)
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, multifunctionFakeAddress2).Times(0)
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, multifunctionFakeAddress2).Return(fakeDriver, nil).Times(1)
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, multifunctionFakeAddress2).Times(0)
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, multifunctionFakeAddress2).Return(multiFunctionFakeID, nil).Times(1)
		// Allow the regular functions to be called for all the other devices, they're harmless.
		// Just force the driver to NOT vfio-pci to ensure they all get ignored.
		mockPCI.EXPECT().GetDeviceIOMMUGroup(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().GetDeviceDriver(pciBasePath, gomock.Any()).Return("definitely-not-vfio-pci", nil).AnyTimes()
		mockPCI.EXPECT().GetDeviceNumaNode(pciBasePath, gomock.Any()).AnyTimes()
		mockPCI.EXPECT().GetDevicePCIID(pciBasePath, gomock.Any()).AnyTimes()

		By("creating a list of fake device using the yaml decoder")
		fakePermittedHostDevicesConfig = `
pciHostDevices:
- pciVendorSelector: "` + singleFunctionFakeID + `"
  resourceName: "` + fakeName + `"
- pciVendorSelector: "` + multiFunctionFakeID + `"
  resourceName: "` + multiFunctionFakeName + `"
  numberOfFunctions: 2
`
		err = yaml.NewYAMLOrJSONDecoder(strings.NewReader(fakePermittedHostDevicesConfig), 1024).Decode(&fakePermittedHostDevices)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakePermittedHostDevices.PciHostDevices).To(HaveLen(2))
		Expect(fakePermittedHostDevices.PciHostDevices[0].PCIVendorSelector).To(Equal(singleFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[0].ResourceName).To(Equal(fakeName))
		Expect(fakePermittedHostDevices.PciHostDevices[1].PCIVendorSelector).To(Equal(multiFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[1].ResourceName).To(Equal(multiFunctionFakeName))
	})

	It("Should parse the permitted devices and find 2 matching PCI devices", func() {
		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices)
		Expect(err).ToNot(HaveOccurred())
		// discoverPermittedHostPCIDevices() will walk real PCI devices wherever the tests are running
		// It's assumed here that it will find the multi-function PCI device at multifunctionFakeAddress 1 and 2
		devices := discoverPermittedHostPCIDevices(supportedPCIDeviceMap)
		Expect(devices).To(HaveLen(2), "one single-function and one multi-function PCI devices are expected to be found")
		Expect(devices[fakeName].devices).To(HaveLen(1), "one single-function PCI device is expected to be found")
		Expect(devices[fakeName].devices[0].pciID).To(Equal(singleFunctionFakeID))
		Expect(devices[fakeName].devices[0].driver).To(Equal(fakeDriver))
		Expect(devices[fakeName].devices[0].pciAddress).To(Equal(fakeAddress))
		Expect(devices[fakeName].devices[0].iommuGroup).To(Equal(fakeIommuGroup))
		Expect(devices[fakeName].devices[0].numaNode).To(Equal(fakeNumaNode))
		Expect(devices[multiFunctionFakeName].devices).To(HaveLen(1), "one multi-function PCI device is expected to be found")
		Expect(devices[multiFunctionFakeName].devices[0].pciID).To(Equal(multiFunctionFakeID))
		Expect(devices[multiFunctionFakeName].devices[0].driver).To(Equal(fakeDriver))
		Expect(devices[multiFunctionFakeName].devices[0].pciAddress).To(Equal(multiFunctionFakeAddress1))
		Expect(devices[multiFunctionFakeName].devices[0].iommuGroup).To(Equal(fakeIommuGroup))
		Expect(devices[multiFunctionFakeName].devices[0].numaNode).To(Equal(fakeNumaNode))
	})

	It("Should validate DPI devices", func() {
		iommuToPCIMap := make(map[string]string)
		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices)
		Expect(err).ToNot(HaveOccurred())
		// discoverPermittedHostPCIDevices() will walk real PCI devices wherever the tests are running
		// It's assumed here that it will find the single-function and multi-function PCI devices at fakeAddress and multifunctionFakeAddress 1 and 2
		devices := discoverPermittedHostPCIDevices(supportedPCIDeviceMap)
		devs := constructDPIdevices(devices[multiFunctionFakeName].devices, iommuToPCIMap)
		Expect(devs[0].ID).To(Equal(fakeIommuGroup))
		Expect(devs[0].Topology.Nodes[0].ID).To(Equal(int64(fakeNumaNode)))
		devs = constructDPIdevices(devices[fakeName].devices, iommuToPCIMap)
		Expect(devs[0].ID).To(Equal(fakeIommuGroup))
		Expect(devs[0].Topology.Nodes[0].ID).To(Equal(int64(fakeNumaNode)))
	})
})

var _ = Describe("config tests", func() {
	It("should fail to validate config when same vendor selector is registering single-function and multi-function devices", func() {
		fakePermittedHostDevicesConfig := `
pciHostDevices:
- pciVendorSelector: "` + singleFunctionFakeID + `"
  resourceName: "` + fakeName + `"
- pciVendorSelector: "` + singleFunctionFakeID + `"
  resourceName: "` + multiFunctionFakeName + `"
  numberOfFunctions: 2
`
		var fakePermittedHostDevices v1.PermittedHostDevices
		err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(fakePermittedHostDevicesConfig), 1024).Decode(&fakePermittedHostDevices)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakePermittedHostDevices.PciHostDevices).To(HaveLen(2))
		Expect(fakePermittedHostDevices.PciHostDevices[0].PCIVendorSelector).To(Equal(singleFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[0].ResourceName).To(Equal(fakeName))
		Expect(fakePermittedHostDevices.PciHostDevices[1].PCIVendorSelector).To(Equal(singleFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[1].ResourceName).To(Equal(multiFunctionFakeName))

		_, err = validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices)
		Expect(err).To(HaveOccurred())
	})

	It("should output a single device when 2 identical single-function devices are configured", func() {
		fakePermittedHostDevicesConfig := `
pciHostDevices:
- pciVendorSelector: "` + singleFunctionFakeID + `"
  resourceName: "` + fakeName + `"
- pciVendorSelector: "` + singleFunctionFakeID + `"
  resourceName: "` + fakeName + `"
`
		var fakePermittedHostDevices v1.PermittedHostDevices
		err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(fakePermittedHostDevicesConfig), 1024).Decode(&fakePermittedHostDevices)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakePermittedHostDevices.PciHostDevices).To(HaveLen(2))
		Expect(fakePermittedHostDevices.PciHostDevices[0].PCIVendorSelector).To(Equal(singleFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[0].ResourceName).To(Equal(fakeName))
		Expect(fakePermittedHostDevices.PciHostDevices[1].PCIVendorSelector).To(Equal(singleFunctionFakeID))
		Expect(fakePermittedHostDevices.PciHostDevices[1].ResourceName).To(Equal(fakeName))

		supportedPCIDeviceMap, err := validatePciHostDevicesConfiguration(fakePermittedHostDevices.PciHostDevices)
		Expect(err).ToNot(HaveOccurred())

		Expect(supportedPCIDeviceMap).To(HaveLen(1))
		Expect(supportedPCIDeviceMap[singleFunctionFakeID].pciResourceVariants).To(HaveLen(1))
	})
})
