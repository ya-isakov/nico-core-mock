package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"

	forgev1 "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

const (
	defaultState     = "Ready"
	defaultSegmentID = "00000000-0000-4000-9000-000000000000"
)

// File is the top-level machines configuration loaded from YAML.
type File struct {
	Machines         []MachineSpec         `yaml:"machines"`
	ExpectedMachines []ExpectedMachineSpec `yaml:"expected_machines"`
}

// MachineSpec describes a NICo machine returned by FindMachinesByIds.
type MachineSpec struct {
	ID                   string         `yaml:"id"`
	State                string         `yaml:"state"`
	Interfaces           []InterfaceSpec `yaml:"interfaces"`
	DiscoveryInfo        map[string]any          `yaml:"discovery_info"`
	DiscoveryInfoFile    string                  `yaml:"discovery_info_file"`
	AttachedDpuMachineID string                  `yaml:"attached_dpu_machine_id"`
	MachineCapabilities  []MachineCapabilitySpec `yaml:"machine_capabilities"`
}

// MachineCapabilitySpec is a single entry in the machine_capabilities YAML list.
// Field names match the REST API machineCapabilities format.
type MachineCapabilitySpec struct {
	Type            string   `yaml:"type"`
	Name            string   `yaml:"name"`
	Vendor          string   `yaml:"vendor"`
	Count           uint32   `yaml:"count"`
	Frequency       string   `yaml:"frequency"`
	Capacity        string   `yaml:"capacity"`
	Cores           uint32   `yaml:"cores"`
	Threads         uint32   `yaml:"threads"`
	DeviceType      string   `yaml:"device_type"`
	InactiveDevices []uint32 `yaml:"inactive_devices"`
}

// InterfaceSpec describes a machine network interface.
type InterfaceSpec struct {
	ID                   string   `yaml:"id"`
	Hostname             string   `yaml:"hostname"`
	MACAddress           string   `yaml:"mac_address"`
	Addresses            []string `yaml:"addresses"`
	SegmentID            string   `yaml:"segment_id"`
	PrimaryInterface     *bool    `yaml:"primary_interface"`
	AttachedDpuMachineID string   `yaml:"attached_dpu_machine_id"`
}

// ExpectedMachineSpec describes inventory expected machines.
type ExpectedMachineSpec struct {
	ID                  string `yaml:"id"`
	BMCMacAddress       string `yaml:"bmc_mac_address"`
	BMCUsername         string `yaml:"bmc_username"`
	BMCPassword         string `yaml:"bmc_password"`
	ChassisSerialNumber string `yaml:"chassis_serial_number"`
	SkuID               string `yaml:"sku_id"`
	BMCIPAddress        string `yaml:"bmc_ip_address"`
	MachineID           string `yaml:"machine_id"`
}

// Inventory holds parsed machines ready to serve over gRPC.
type Inventory struct {
	Machines                 map[string]*forgev1.Machine
	ExpectedMachines         []*forgev1.ExpectedMachine
	LinkedExpectedMachines   []*forgev1.LinkedExpectedMachine
}

// Load reads and validates a machines YAML file.
//
// Accepted shapes:
//   - plain inventory: { machines: [...], expected_machines?: [...] }
//   - Helm values.yaml: { inventory: { machines: [...] }, ... } (other keys ignored)
func Load(path string) (*Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var file File
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if len(file.Machines) == 0 {
		var wrapped struct {
			Inventory File `yaml:"inventory"`
		}
		if err := yaml.Unmarshal(data, &wrapped); err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
		file = wrapped.Inventory
	}
	if len(file.Machines) == 0 {
		return nil, fmt.Errorf("config %q: at least one machine is required", path)
	}

	baseDir := dirOf(path)
	inv := &Inventory{
		Machines: make(map[string]*forgev1.Machine, len(file.Machines)),
	}

	for i, spec := range file.Machines {
		machine, err := spec.toProto(baseDir, i)
		if err != nil {
			return nil, fmt.Errorf("machine[%d]: %w", i, err)
		}
		id := machine.GetId().GetId()
		if _, exists := inv.Machines[id]; exists {
			return nil, fmt.Errorf("machine[%d]: duplicate machine id %q", i, id)
		}
		inv.Machines[id] = machine
	}

	for i, spec := range file.ExpectedMachines {
		em, linked, err := spec.toProto()
		if err != nil {
			return nil, fmt.Errorf("expected_machines[%d]: %w", i, err)
		}
		inv.ExpectedMachines = append(inv.ExpectedMachines, em)
		if linked != nil {
			inv.LinkedExpectedMachines = append(inv.LinkedExpectedMachines, linked)
		}
	}

	return inv, nil
}

func (spec MachineSpec) toProto(baseDir string, index int) (*forgev1.Machine, error) {
	id := spec.ID
	if id == "" {
		id = uuid.NewString()
	}

	state := spec.State
	if state == "" {
		state = defaultState
	}

	interfaces := spec.Interfaces
	if len(interfaces) == 0 {
		interfaces = []InterfaceSpec{{
			Hostname:         fmt.Sprintf("mock-host-%d.nico.local", index),
			MACAddress:       randomMAC(index),
			Addresses:        []string{fmt.Sprintf("10.10.%d.%d", index/254+1, index%254+1)},
			SegmentID:        defaultSegmentID,
			PrimaryInterface: boolPtr(true),
		}}
	}

	protoIfaces := make([]*forgev1.MachineInterface, 0, len(interfaces))
	for _, iface := range interfaces {
		pi, err := iface.toProto(id, spec.AttachedDpuMachineID)
		if err != nil {
			return nil, err
		}
		protoIfaces = append(protoIfaces, pi)
	}

	discovery, err := loadDiscoveryInfo(baseDir, spec.DiscoveryInfo, spec.DiscoveryInfoFile)
	if err != nil {
		return nil, err
	}

	return &forgev1.Machine{
		Id:            &forgev1.MachineId{Id: id},
		State:         state,
		Interfaces:    protoIfaces,
		DiscoveryInfo: discovery,
		Capabilities:  machineCapabilitiesToProto(spec.MachineCapabilities),
	}, nil
}

func (spec InterfaceSpec) toProto(machineID, defaultDPU string) (*forgev1.MachineInterface, error) {
	ifaceID := spec.ID
	if ifaceID == "" {
		ifaceID = uuid.NewString()
	}

	segmentID := spec.SegmentID
	if segmentID == "" {
		segmentID = defaultSegmentID
	}

	mac := spec.MACAddress
	if mac == "" {
		return nil, fmt.Errorf("interface for machine %q requires mac_address", machineID)
	}

	primary := true
	if spec.PrimaryInterface != nil {
		primary = *spec.PrimaryInterface
	}

	dpuID := spec.AttachedDpuMachineID
	if dpuID == "" {
		dpuID = defaultDPU
	}

	iface := &forgev1.MachineInterface{
		Id:               &forgev1.MachineInterfaceId{Value: ifaceID},
		MachineId:        &forgev1.MachineId{Id: machineID},
		SegmentId:        &forgev1.NetworkSegmentId{Value: segmentID},
		Hostname:         spec.Hostname,
		PrimaryInterface: primary,
		MacAddress:       mac,
		Address:          append([]string(nil), spec.Addresses...),
	}
	if dpuID != "" {
		iface.AttachedDpuMachineId = &forgev1.MachineId{Id: dpuID}
	}
	return iface, nil
}

func (spec ExpectedMachineSpec) toProto() (*forgev1.ExpectedMachine, *forgev1.LinkedExpectedMachine, error) {
	id := spec.ID
	if id == "" {
		id = uuid.NewString()
	}
	if spec.BMCMacAddress == "" {
		return nil, nil, fmt.Errorf("bmc_mac_address is required")
	}

	em := &forgev1.ExpectedMachine{
		Id:                  &forgev1.UUID{Value: id},
		BmcMacAddress:       spec.BMCMacAddress,
		BmcUsername:         spec.BMCUsername,
		BmcPassword:         spec.BMCPassword,
		ChassisSerialNumber: spec.ChassisSerialNumber,
		SkuId:               strPtr(spec.SkuID),
		BmcIpAddress:        strPtr(spec.BMCIPAddress),
	}

	linked := &forgev1.LinkedExpectedMachine{
		ChassisSerialNumber: spec.ChassisSerialNumber,
		BmcMacAddress:       spec.BMCMacAddress,
		ExpectedMachineId:   &forgev1.UUID{Value: id},
	}
	if spec.MachineID != "" {
		linked.MachineId = &forgev1.MachineId{Id: spec.MachineID}
	}

	return em, linked, nil
}

func loadDiscoveryInfo(baseDir string, inline map[string]any, filePath string) (*forgev1.DiscoveryInfo, error) {
	switch {
	case filePath != "":
		if !isAbs(filePath) {
			filePath = joinPath(baseDir, filePath)
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read discovery_info_file: %w", err)
		}
		return unmarshalDiscoveryInfo(data)
	case len(inline) > 0:
		raw, err := json.Marshal(inline)
		if err != nil {
			return nil, fmt.Errorf("marshal discovery_info: %w", err)
		}
		return unmarshalDiscoveryInfo(raw)
	default:
		return nil, nil
	}
}

func unmarshalDiscoveryInfo(data []byte) (*forgev1.DiscoveryInfo, error) {
	info := &forgev1.DiscoveryInfo{}
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := opts.Unmarshal(data, info); err != nil {
		return nil, fmt.Errorf("parse discovery info: %w", err)
	}
	return info, nil
}

func machineCapabilitiesToProto(specs []MachineCapabilitySpec) *forgev1.MachineCapabilitiesSet {
	if len(specs) == 0 {
		return nil
	}
	set := &forgev1.MachineCapabilitiesSet{}
	for _, s := range specs {
		switch s.Type {
		case "CPU":
			cpu := &forgev1.MachineCapabilityAttributesCpu{Name: s.Name, Count: s.Count}
			if s.Vendor != "" {
				cpu.Vendor = &s.Vendor
			}
			if s.Cores != 0 {
				cpu.Cores = &s.Cores
			}
			if s.Threads != 0 {
				cpu.Threads = &s.Threads
			}
			set.Cpu = append(set.Cpu, cpu)
		case "GPU":
			gpu := &forgev1.MachineCapabilityAttributesGpu{Name: s.Name, Count: s.Count}
			if s.Vendor != "" {
				gpu.Vendor = &s.Vendor
			}
			if s.Frequency != "" {
				gpu.Frequency = &s.Frequency
			}
			if s.Capacity != "" {
				gpu.Capacity = &s.Capacity
			}
			if s.DeviceType != "" {
				dt := deviceTypeToProto(s.DeviceType)
				gpu.DeviceType = &dt
			}
			set.Gpu = append(set.Gpu, gpu)
		case "Memory":
			mem := &forgev1.MachineCapabilityAttributesMemory{Name: s.Name, Count: s.Count}
			if s.Vendor != "" {
				mem.Vendor = &s.Vendor
			}
			if s.Capacity != "" {
				mem.Capacity = &s.Capacity
			}
			set.Memory = append(set.Memory, mem)
		case "Storage":
			st := &forgev1.MachineCapabilityAttributesStorage{Name: s.Name, Count: s.Count}
			if s.Vendor != "" {
				st.Vendor = &s.Vendor
			}
			if s.Capacity != "" {
				st.Capacity = &s.Capacity
			}
			set.Storage = append(set.Storage, st)
		case "Network":
			net := &forgev1.MachineCapabilityAttributesNetwork{Name: s.Name, Count: s.Count}
			if s.Vendor != "" {
				net.Vendor = &s.Vendor
			}
			if s.DeviceType != "" {
				dt := deviceTypeToProto(s.DeviceType)
				net.DeviceType = &dt
			}
			set.Network = append(set.Network, net)
		case "InfiniBand":
			ib := &forgev1.MachineCapabilityAttributesInfiniband{
				Name:            s.Name,
				Count:           s.Count,
				InactiveDevices: s.InactiveDevices,
			}
			if s.Vendor != "" {
				ib.Vendor = &s.Vendor
			}
			set.Infiniband = append(set.Infiniband, ib)
		case "DPU":
			set.Dpu = append(set.Dpu, &forgev1.MachineCapabilityAttributesDpu{
				Name:  s.Name,
				Count: s.Count,
			})
		}
	}
	return set
}

func deviceTypeToProto(s string) forgev1.MachineCapabilityDeviceType {
	switch s {
	case "DPU":
		return forgev1.MachineCapabilityDeviceType_MACHINE_CAPABILITY_DEVICE_TYPE_DPU
	case "NVLink":
		return forgev1.MachineCapabilityDeviceType_MACHINE_CAPABILITY_DEVICE_TYPE_NVLINK
	default:
		return forgev1.MachineCapabilityDeviceType_MACHINE_CAPABILITY_DEVICE_TYPE_UNKNOWN
	}
}

func randomMAC(index int) string {
	return fmt.Sprintf("02:00:00:00:%02x:%02x", (index>>8)&0xff, index&0xff)
}

func boolPtr(v bool) *bool {
	return &v
}

func strPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

func isAbs(path string) bool {
	return len(path) > 0 && path[0] == '/'
}

func joinPath(base, rel string) string {
	if base == "." || base == "" {
		return rel
	}
	return base + "/" + rel
}
