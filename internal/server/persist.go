package server

import (
	"github.com/jumpojoy/nico-core-mock/internal/statestore"
)

func (f *NICoServerImpl) applySnapshot(snap *statestore.Snapshot) {
	if snap == nil {
		return
	}

	for _, vpc := range snap.Vpcs {
		if vpc.GetId() == nil || vpc.GetId().GetValue() == "" {
			continue
		}
		f.v[vpc.GetId().GetValue()] = vpc
	}
	for _, ns := range snap.NetworkSegments {
		if ns.GetId() == nil || ns.GetId().GetValue() == "" {
			continue
		}
		f.ns[ns.GetId().GetValue()] = ns
	}
	for _, ins := range snap.Instances {
		if ins.GetId() == nil || ins.GetId().GetValue() == "" {
			continue
		}
		f.ins[ins.GetId().GetValue()] = ins
	}
	for _, machine := range snap.Machines {
		if machine.GetId() == nil || machine.GetId().GetId() == "" {
			continue
		}
		f.m[machine.GetId().GetId()] = machine
	}
	for _, prefix := range snap.VpcPrefixes {
		if prefix.GetId() == nil || prefix.GetId().GetValue() == "" {
			continue
		}
		f.vp[prefix.GetId().GetValue()] = prefix
	}
	for _, peering := range snap.VpcPeerings {
		if peering.GetId() == nil || peering.GetId().GetValue() == "" {
			continue
		}
		f.vpp[peering.GetId().GetValue()] = peering
	}
	for _, image := range snap.OsImages {
		if image.GetAttributes() == nil || image.GetAttributes().GetId() == nil || image.GetAttributes().GetId().GetValue() == "" {
			continue
		}
		f.osi[image.GetAttributes().GetId().GetValue()] = image
	}
	for _, os := range snap.OperatingSystems {
		if os.GetId() == nil || os.GetId().GetValue() == "" {
			continue
		}
		f.oss[os.GetId().GetValue()] = os
	}
	for _, it := range snap.InstanceTypes {
		if it.GetId() == "" {
			continue
		}
		f.it[it.GetId()] = it
	}
}

func (f *NICoServerImpl) exportSnapshot() *statestore.Snapshot {
	return &statestore.Snapshot{
		Version:         statestore.SnapshotVersion(),
		Vpcs:            mapValues(f.v),
		NetworkSegments: mapValues(f.ns),
		Instances:       mapValues(f.ins),
		Machines:        mapValues(f.m),
		VpcPrefixes:     mapValues(f.vp),
		VpcPeerings:     mapValues(f.vpp),
		OsImages:         mapValues(f.osi),
		OperatingSystems: mapValues(f.oss),
		InstanceTypes:    mapValues(f.it),
	}
}

func (f *NICoServerImpl) persistState() error {
	if f.stateFile == "" {
		return nil
	}
	return statestore.Save(f.stateFile, f.exportSnapshot())
}

func (f *NICoServerImpl) loadPersistedState() error {
	if f.stateFile == "" {
		return nil
	}
	snap, err := statestore.Load(f.stateFile)
	if err != nil {
		return err
	}
	f.applySnapshot(snap)
	return nil
}

func mapValues[K comparable, V any](m map[K]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
