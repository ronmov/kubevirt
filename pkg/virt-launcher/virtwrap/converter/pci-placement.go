package converter

import (
	"fmt"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

// iteratePCIAddresses invokes the callback function for each PCI device specified in the domain
func iteratePCIAddresses(spec *api.DomainSpec, callback func(address *api.Address) (*api.Address, error)) (err error) {
	fn := func(address *api.Address) (*api.Address, error) {
		if address == nil || address.Type == "" || address.Type == api.AddressPCI {
			return callback(address)
		}
		return address, nil
	}
	for i, iface := range spec.Devices.Interfaces {
		spec.Devices.Interfaces[i].Address, err = fn(iface.Address)
		if err != nil {
			return err
		}
	}
	for i, hostDev := range spec.Devices.HostDevices {
		if hostDev.Type != api.HostDevicePCI {
			continue
		}
		spec.Devices.HostDevices[i].Address, err = fn(hostDev.Address)
		if err != nil {
			return err
		}
	}
	for i, controller := range spec.Devices.Controllers {
		// pci-root and pcie-root devices can by definition hot have a pci address on its own
		if controller.Model == "pci-root" || controller.Model == "pcie-root" {
			continue
		}
		spec.Devices.Controllers[i].Address, err = fn(controller.Address)
		if err != nil {
			return err
		}
	}
	for i, disk := range spec.Devices.Disks {
		if disk.Target.Bus != v1.DiskBusVirtio {
			continue
		}
		spec.Devices.Disks[i].Address, err = fn(disk.Address)
		if err != nil {
			return err
		}
	}
	for i, input := range spec.Devices.Inputs {
		if input.Bus != v1.VirtIO {
			continue
		}
		spec.Devices.Inputs[i].Address, err = fn(input.Address)
		if err != nil {
			return err
		}
	}
	for i, watchdog := range spec.Devices.Watchdogs {
		spec.Devices.Watchdogs[i].Address, err = fn(watchdog.Address)
		if err != nil {
			return err
		}
	}
	if spec.Devices.Rng != nil {
		spec.Devices.Rng.Address, err = fn(spec.Devices.Rng.Address)
		if err != nil {
			return err
		}
	}
	if spec.Devices.Ballooning != nil {
		spec.Devices.Ballooning.Address, err = fn(spec.Devices.Ballooning.Address)
		if err != nil {
			return err
		}
	}
	return nil
}

func CountPCIDevices(spec *api.DomainSpec) (count int, err error) {
	err = iteratePCIAddresses(spec, func(address *api.Address) (*api.Address, error) {
		count++
		return address, nil
	})
	return count, err
}

func PlacePCIDevicesOnRootComplex(spec *api.DomainSpec, multiFunctionHostDevices []api.HostDevice) error {
	assigner := newRootSlotAssigner()
	err := iteratePCIAddresses(spec, assigner.PlacePCIDeviceAtNextSlot)
	if err != nil {
		return err
	}

	// multiFunctionHostDevices is sorted and always contains a device with function 0
	for i := range multiFunctionHostDevices {
		addr, err := assigner.PlacePCIDeviceAtNextBusPreserveFunction(multiFunctionHostDevices[i].Source.Address)
		if err != nil {
			return err
		}
		multiFunctionHostDevices[i].Address = addr
	}

	spec.Devices.HostDevices = append(spec.Devices.HostDevices, multiFunctionHostDevices...)

	return nil
}

func (p *pciRootSlotAssigner) nextSlot() (int, error) {
	slot := p.slot + 1
	// reserved slots are:
	// slot 0
	// slot 1 for VGA
	// slot 0x1f for a sata controller from  qemu
	// slot 0x1b for the first ich9 sound card
	switch slot {
	case 0, 0x01:
		slot = 0x02
	case 0x1f, 0x1b:
		slot = slot + 1
	}

	if slot >= 0x20 {
		return slot, fmt.Errorf("no space left on the root PCI bus")
	}
	p.slot = slot
	return slot, nil
}

func (p *pciRootSlotAssigner) nextBus() (int, error) {
	bus := p.bus + 1

	if bus >= 0x20 { // TODO: correct this number
		return bus, fmt.Errorf("max pci bus allocation reached")
	}
	p.bus = bus
	return bus, nil
}

func newRootSlotAssigner() *pciRootSlotAssigner {
	return &pciRootSlotAssigner{slot: -1, bus: 0}
}

type pciRootSlotAssigner struct {
	slot int
	bus  int
}

func (p *pciRootSlotAssigner) PlacePCIDeviceAtNextBusPreserveFunction(sourceAddress *api.Address) (*api.Address, error) {
	if sourceAddress == nil {
		return nil, fmt.Errorf("when preserving function, sourceAddress parameter must have value")
	}

	bus := ""
	if sourceAddress.Function == "0" || sourceAddress.Function == "0x0" {
		busNum, err := p.nextBus()
		if err != nil {
			return nil, err
		}
		bus = fmt.Sprintf("%#02x", busNum)
	}
	if bus == "" {
		bus = fmt.Sprintf("%#02x", p.bus)
	}

	address := api.Address{}
	address.Type = api.AddressPCI
	address.Domain = "0x0000"
	address.Bus = bus
	address.Slot = "0x00"
	address.Function = sourceAddress.Function
	return &address, nil
}

func (p *pciRootSlotAssigner) PlacePCIDeviceAtNextSlot(address *api.Address) (*api.Address, error) {
	if address == nil {
		address = &api.Address{}
	}

	// keep explicit requests for pci addresses
	if address.Domain != "" {
		return address, nil
	}

	slot, err := p.nextSlot()
	if err != nil {
		return nil, err
	}
	address.Type = api.AddressPCI
	address.Domain = "0x0000"
	address.Bus = "0x00"
	address.Slot = fmt.Sprintf("%#02x", slot)
	address.Function = "0x0"
	return address, nil
}
