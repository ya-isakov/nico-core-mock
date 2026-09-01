package server

import (
	"context"
	"testing"

	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
	"google.golang.org/protobuf/types/known/emptypb"

	libvirtfilter "github.com/jumpojoy/nico-core-mock/internal/libvirt"
)

// GetAllExpectedMachinesLinked must link each expected machine to the distinct
// visible machine that carries its BMC MAC, and leave unmatched expected
// machines unlinked — rather than collapsing every one onto the first machine.
func TestGetAllExpectedMachinesLinked_MatchesByMAC(t *testing.T) {
	t.Parallel()

	const (
		mac0 = "58:a2:e1:5b:d1:b0"
		mac1 = "58:a2:e1:5b:d1:b1"
	)

	srv := &NICoServerImpl{
		powerChecker: libvirtfilter.NoopChecker{},
		m: map[string]*cwssaws.Machine{
			"machine-0": {
				Id:         &cwssaws.MachineId{Id: "machine-0"},
				Interfaces: []*cwssaws.MachineInterface{{MacAddress: mac0}},
			},
			"machine-1": {
				Id:         &cwssaws.MachineId{Id: "machine-1"},
				Interfaces: []*cwssaws.MachineInterface{{MacAddress: mac1}},
			},
		},
		em: map[string]*cwssaws.ExpectedMachine{
			"em-0": {Id: &cwssaws.UUID{Value: "em-0"}, BmcMacAddress: mac0, ChassisSerialNumber: "A0000"},
			"em-1": {Id: &cwssaws.UUID{Value: "em-1"}, BmcMacAddress: mac1, ChassisSerialNumber: "A0001"},
			// No machine carries this MAC -> must stay unlinked.
			"em-x": {Id: &cwssaws.UUID{Value: "em-x"}, BmcMacAddress: "de:ad:be:ef:00:00", ChassisSerialNumber: "AXXXX"},
		},
	}

	out, err := srv.GetAllExpectedMachinesLinked(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("GetAllExpectedMachinesLinked() error = %v", err)
	}

	got := make(map[string]string, len(out.GetExpectedMachines()))
	for _, l := range out.GetExpectedMachines() {
		got[l.GetExpectedMachineId().GetValue()] = l.GetMachineId().GetId()
	}

	want := map[string]string{
		"em-0": "machine-0",
		"em-1": "machine-1",
		"em-x": "", // unmatched -> nil MachineId
	}
	for emID, wantMID := range want {
		if got[emID] != wantMID {
			t.Errorf("expected machine %q linked to %q, want %q", emID, got[emID], wantMID)
		}
	}

	// Distinct expected machines must not collapse onto the same machine id.
	if got["em-0"] == got["em-1"] {
		t.Errorf("em-0 and em-1 both linked to %q; expected distinct machines", got["em-0"])
	}
}

// A non-empty YAML-seeded link list must not hide expected machines created at
// runtime: seeded links are returned verbatim, and uncovered dynamic expected
// machines are appended (linked by BMC MAC).
func TestGetAllExpectedMachinesLinked_MergesSeedAndDynamic(t *testing.T) {
	t.Parallel()

	const (
		mac0 = "58:a2:e1:5b:d1:b0"
		mac1 = "58:a2:e1:5b:d1:b1"
	)

	srv := &NICoServerImpl{
		powerChecker: libvirtfilter.NoopChecker{},
		m: map[string]*cwssaws.Machine{
			"machine-1": {
				Id:         &cwssaws.MachineId{Id: "machine-1"},
				Interfaces: []*cwssaws.MachineInterface{{MacAddress: mac1}},
			},
		},
		// Seeded link with explicit machine_id (e.g. from inventory YAML).
		linkedExpectedMachines: []*cwssaws.LinkedExpectedMachine{
			{
				ExpectedMachineId: &cwssaws.UUID{Value: "em-seed"},
				BmcMacAddress:     mac0,
				MachineId:         &cwssaws.MachineId{Id: "seed-machine"},
			},
		},
		em: map[string]*cwssaws.ExpectedMachine{
			// Same id as the seeded link -> must not be duplicated.
			"em-seed": {Id: &cwssaws.UUID{Value: "em-seed"}, BmcMacAddress: mac0, ChassisSerialNumber: "A0000"},
			// Runtime-created EM not in the seed -> appended, linked by MAC.
			"em-dyn": {Id: &cwssaws.UUID{Value: "em-dyn"}, BmcMacAddress: mac1, ChassisSerialNumber: "A0001"},
		},
	}

	out, err := srv.GetAllExpectedMachinesLinked(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("GetAllExpectedMachinesLinked() error = %v", err)
	}

	got := make(map[string]string, len(out.GetExpectedMachines()))
	for _, l := range out.GetExpectedMachines() {
		id := l.GetExpectedMachineId().GetValue()
		if _, dup := got[id]; dup {
			t.Errorf("expected machine %q listed more than once", id)
		}
		got[id] = l.GetMachineId().GetId()
	}

	if len(got) != 2 {
		t.Fatalf("got %d expected machines, want 2: %v", len(got), got)
	}
	if got["em-seed"] != "seed-machine" {
		t.Errorf("seeded link em-seed = %q, want %q (explicit YAML link must win)", got["em-seed"], "seed-machine")
	}
	if got["em-dyn"] != "machine-1" {
		t.Errorf("dynamic em-dyn = %q, want %q (linked by MAC)", got["em-dyn"], "machine-1")
	}
}

// CreateVpcPeering must persist the pair Cloud asked for under Cloud's own ID,
// so the peering that inventory discovery reports back is the same object.
func TestVpcPeeringLifecycle(t *testing.T) {
	t.Parallel()

	const (
		peeringID = "10f31516-cf0c-4048-97b2-129d8f8f45db"
		vpcA      = "63b513fa-e8d9-43f0-adfa-673f10194c97"
		vpcB      = "07fa3ea1-5795-4156-bd02-090406006e13"
	)

	srv := &NICoServerImpl{vpp: map[string]*cwssaws.VpcPeering{}}
	ctx := context.Background()
	req := &cwssaws.VpcPeeringCreationRequest{
		Id:        &cwssaws.VpcPeeringId{Value: peeringID},
		VpcId:     &cwssaws.VpcId{Value: vpcA},
		PeerVpcId: &cwssaws.VpcId{Value: vpcB},
	}

	p, err := srv.CreateVpcPeering(ctx, req)
	if err != nil {
		t.Fatalf("CreateVpcPeering() error = %v", err)
	}
	if p.GetId().GetValue() != peeringID {
		t.Errorf("peering ID = %q, want %q", p.GetId().GetValue(), peeringID)
	}
	if p.GetVpcId().GetValue() != vpcA || p.GetPeerVpcId().GetValue() != vpcB {
		t.Errorf("peering pair = (%q, %q), want (%q, %q)",
			p.GetVpcId().GetValue(), p.GetPeerVpcId().GetValue(), vpcA, vpcB)
	}

	// The same pair in reverse order is the same peering, not a second one.
	if _, err := srv.CreateVpcPeering(ctx, &cwssaws.VpcPeeringCreationRequest{
		Id:        &cwssaws.VpcPeeringId{Value: "other-id"},
		VpcId:     &cwssaws.VpcId{Value: vpcB},
		PeerVpcId: &cwssaws.VpcId{Value: vpcA},
	}); err == nil {
		t.Error("CreateVpcPeering() with reversed pair succeeded, want AlreadyExists")
	}

	// Discovery must see it from either side of the pair, and only from those.
	for _, tc := range []struct {
		filter string
		want   int
	}{{vpcA, 1}, {vpcB, 1}, {"", 1}, {"unrelated-vpc", 0}} {
		f := &cwssaws.VpcPeeringSearchFilter{}
		if tc.filter != "" {
			f.VpcId = &cwssaws.VpcId{Value: tc.filter}
		}
		ids, err := srv.FindVpcPeeringIds(ctx, f)
		if err != nil {
			t.Fatalf("FindVpcPeeringIds(%q) error = %v", tc.filter, err)
		}
		if got := len(ids.GetVpcPeeringIds()); got != tc.want {
			t.Errorf("FindVpcPeeringIds(%q) returned %d ids, want %d", tc.filter, got, tc.want)
		}
	}

	list, err := srv.FindVpcPeeringsByIds(ctx, &cwssaws.VpcPeeringsByIdsRequest{
		VpcPeeringIds: []*cwssaws.VpcPeeringId{{Value: peeringID}},
	})
	if err != nil {
		t.Fatalf("FindVpcPeeringsByIds() error = %v", err)
	}
	if len(list.GetVpcPeerings()) != 1 {
		t.Fatalf("FindVpcPeeringsByIds() returned %d peerings, want 1", len(list.GetVpcPeerings()))
	}

	if _, err := srv.DeleteVpcPeering(ctx, &cwssaws.VpcPeeringDeletionRequest{
		Id: &cwssaws.VpcPeeringId{Value: peeringID},
	}); err != nil {
		t.Fatalf("DeleteVpcPeering() error = %v", err)
	}
	ids, err := srv.FindVpcPeeringIds(ctx, &cwssaws.VpcPeeringSearchFilter{})
	if err != nil {
		t.Fatalf("FindVpcPeeringIds() after delete error = %v", err)
	}
	if len(ids.GetVpcPeeringIds()) != 0 {
		t.Errorf("FindVpcPeeringIds() after delete returned %d ids, want 0", len(ids.GetVpcPeeringIds()))
	}
}
