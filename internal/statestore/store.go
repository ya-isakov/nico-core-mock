package statestore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

const stateVersion = 1

// SnapshotVersion returns the current on-disk snapshot schema version.
func SnapshotVersion() int { return stateVersion }

// Snapshot is the on-disk representation of mutable mock Forge state.
type Snapshot struct {
	Version         int                         `json:"version"`
	Vpcs            []*cwssaws.Vpc              `json:"vpcs,omitempty"`
	NetworkSegments []*cwssaws.NetworkSegment   `json:"network_segments,omitempty"`
	Instances       []*cwssaws.Instance         `json:"instances,omitempty"`
	Machines        []*cwssaws.Machine          `json:"machines,omitempty"`
	VpcPrefixes     []*cwssaws.VpcPrefix        `json:"vpc_prefixes,omitempty"`
	VpcPeerings     []*cwssaws.VpcPeering       `json:"vpc_peerings,omitempty"`
	OsImages          []*cwssaws.OsImage          `json:"os_images,omitempty"`
	OperatingSystems  []*cwssaws.OperatingSystem  `json:"operating_systems,omitempty"`
	InstanceTypes     []*cwssaws.InstanceType     `json:"instance_types,omitempty"`
}

type snapshotWire struct {
	Version         int               `json:"version"`
	Vpcs            []json.RawMessage `json:"vpcs,omitempty"`
	NetworkSegments []json.RawMessage `json:"network_segments,omitempty"`
	Instances       []json.RawMessage `json:"instances,omitempty"`
	Machines        []json.RawMessage `json:"machines,omitempty"`
	VpcPrefixes     []json.RawMessage `json:"vpc_prefixes,omitempty"`
	VpcPeerings     []json.RawMessage `json:"vpc_peerings,omitempty"`
	OsImages          []json.RawMessage `json:"os_images,omitempty"`
	OperatingSystems  []json.RawMessage `json:"operating_systems,omitempty"`
	InstanceTypes     []json.RawMessage `json:"instance_types,omitempty"`
}

var protoJSON = protojson.MarshalOptions{
	EmitUnpopulated: false,
	UseProtoNames:   true,
}

var protoJSONUnmarshal = protojson.UnmarshalOptions{
	DiscardUnknown: true,
}

func marshalProtoSlice[T proto.Message](items []T) ([]json.RawMessage, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]json.RawMessage, len(items))
	for i, item := range items {
		data, err := protoJSON.Marshal(item)
		if err != nil {
			return nil, err
		}
		out[i] = data
	}
	return out, nil
}

func unmarshalProtoSlice[T proto.Message](raw []json.RawMessage, zero func() T) ([]T, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]T, 0, len(raw))
	for _, data := range raw {
		msg := zero()
		if err := protoJSONUnmarshal.Unmarshal(data, msg); err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, nil
}

func snapshotToWire(snap *Snapshot) (*snapshotWire, error) {
	vpcs, err := marshalProtoSlice(snap.Vpcs)
	if err != nil {
		return nil, fmt.Errorf("marshal vpcs: %w", err)
	}
	networkSegments, err := marshalProtoSlice(snap.NetworkSegments)
	if err != nil {
		return nil, fmt.Errorf("marshal network_segments: %w", err)
	}
	instances, err := marshalProtoSlice(snap.Instances)
	if err != nil {
		return nil, fmt.Errorf("marshal instances: %w", err)
	}
	machines, err := marshalProtoSlice(snap.Machines)
	if err != nil {
		return nil, fmt.Errorf("marshal machines: %w", err)
	}
	vpcPrefixes, err := marshalProtoSlice(snap.VpcPrefixes)
	if err != nil {
		return nil, fmt.Errorf("marshal vpc_prefixes: %w", err)
	}
	vpcPeerings, err := marshalProtoSlice(snap.VpcPeerings)
	if err != nil {
		return nil, fmt.Errorf("marshal vpc_peerings: %w", err)
	}
	osImages, err := marshalProtoSlice(snap.OsImages)
	if err != nil {
		return nil, fmt.Errorf("marshal os_images: %w", err)
	}
	operatingSystems, err := marshalProtoSlice(snap.OperatingSystems)
	if err != nil {
		return nil, fmt.Errorf("marshal operating_systems: %w", err)
	}
	instanceTypes, err := marshalProtoSlice(snap.InstanceTypes)
	if err != nil {
		return nil, fmt.Errorf("marshal instance_types: %w", err)
	}

	return &snapshotWire{
		Version:          snap.Version,
		Vpcs:             vpcs,
		NetworkSegments:  networkSegments,
		Instances:        instances,
		Machines:         machines,
		VpcPrefixes:      vpcPrefixes,
		VpcPeerings:      vpcPeerings,
		OsImages:           osImages,
		OperatingSystems: operatingSystems,
		InstanceTypes:      instanceTypes,
	}, nil
}

func wireToSnapshot(wire *snapshotWire) (*Snapshot, error) {
	vpcs, err := unmarshalProtoSlice(wire.Vpcs, func() *cwssaws.Vpc { return &cwssaws.Vpc{} })
	if err != nil {
		return nil, fmt.Errorf("unmarshal vpcs: %w", err)
	}
	networkSegments, err := unmarshalProtoSlice(wire.NetworkSegments, func() *cwssaws.NetworkSegment { return &cwssaws.NetworkSegment{} })
	if err != nil {
		return nil, fmt.Errorf("unmarshal network_segments: %w", err)
	}
	instances, err := unmarshalProtoSlice(wire.Instances, func() *cwssaws.Instance { return &cwssaws.Instance{} })
	if err != nil {
		return nil, fmt.Errorf("unmarshal instances: %w", err)
	}
	machines, err := unmarshalProtoSlice(wire.Machines, func() *cwssaws.Machine { return &cwssaws.Machine{} })
	if err != nil {
		return nil, fmt.Errorf("unmarshal machines: %w", err)
	}
	vpcPrefixes, err := unmarshalProtoSlice(wire.VpcPrefixes, func() *cwssaws.VpcPrefix { return &cwssaws.VpcPrefix{} })
	if err != nil {
		return nil, fmt.Errorf("unmarshal vpc_prefixes: %w", err)
	}
	vpcPeerings, err := unmarshalProtoSlice(wire.VpcPeerings, func() *cwssaws.VpcPeering { return &cwssaws.VpcPeering{} })
	if err != nil {
		return nil, fmt.Errorf("unmarshal vpc_peerings: %w", err)
	}
	osImages, err := unmarshalProtoSlice(wire.OsImages, func() *cwssaws.OsImage { return &cwssaws.OsImage{} })
	if err != nil {
		return nil, fmt.Errorf("unmarshal os_images: %w", err)
	}
	operatingSystems, err := unmarshalProtoSlice(wire.OperatingSystems, func() *cwssaws.OperatingSystem { return &cwssaws.OperatingSystem{} })
	if err != nil {
		return nil, fmt.Errorf("unmarshal operating_systems: %w", err)
	}
	instanceTypes, err := unmarshalProtoSlice(wire.InstanceTypes, func() *cwssaws.InstanceType { return &cwssaws.InstanceType{} })
	if err != nil {
		return nil, fmt.Errorf("unmarshal instance_types: %w", err)
	}

	return &Snapshot{
		Version:          wire.Version,
		Vpcs:             vpcs,
		NetworkSegments:  networkSegments,
		Instances:        instances,
		Machines:         machines,
		VpcPrefixes:      vpcPrefixes,
		VpcPeerings:      vpcPeerings,
		OsImages:           osImages,
		OperatingSystems: operatingSystems,
		InstanceTypes:      instanceTypes,
	}, nil
}

// Load reads a snapshot from path. Missing file returns an empty snapshot.
func Load(path string) (*Snapshot, error) {
	if path == "" {
		return &Snapshot{Version: stateVersion}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Snapshot{Version: stateVersion}, nil
		}
		return nil, fmt.Errorf("read state file %q: %w", path, err)
	}

	wire := &snapshotWire{}
	if err := json.Unmarshal(data, wire); err != nil {
		return nil, fmt.Errorf("parse state file %q: %w", path, err)
	}

	snap, err := wireToSnapshot(wire)
	if err != nil {
		return nil, fmt.Errorf("parse state file %q: %w", path, err)
	}
	if snap.Version == 0 {
		snap.Version = stateVersion
	}
	return snap, nil
}

// Save writes snap atomically to path.
func Save(path string, snap *Snapshot) error {
	if path == "" {
		return nil
	}
	if snap == nil {
		snap = &Snapshot{Version: stateVersion}
	}
	if snap.Version == 0 {
		snap.Version = stateVersion
	}

	wire, err := snapshotToWire(snap)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create state directory %q: %w", dir, err)
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write state temp file %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace state file %q: %w", path, err)
	}
	return nil
}
