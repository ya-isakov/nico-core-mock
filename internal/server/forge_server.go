// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gogo/status"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	emptypb "google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/rs/zerolog/log"

	"github.com/jumpojoy/nico-core-mock/internal/config"
	libvirtfilter "github.com/jumpojoy/nico-core-mock/internal/libvirt"

	"github.com/NVIDIA/infra-controller/rest-api/site-workflow/pkg/grpc/mockdata"
	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

var (
	// DefaultPort is the default port that the server listens at
	DefaultPort = ":11079"
	// DefaultVpcId is the default VPC ID for testing
	DefaultVpcId = "00000000-0000-4000-8000-000000000000"
	// DefaultNetworkSegmentId is the default NetworkSegment ID for testing
	DefaultNetworkSegmentId = "00000000-0000-4000-9000-000000000000"
	// DefaultTenantKeysetId is the default TenantKeyset ID for testing
	DefaultTenantKeysetId = "00000000-0000-4000-a000-000000000000"
	// DefaultIBParitionId is the default IBPartition ID for testing
	DefaultIBParitionId = "00000000-0000-4000-b000-000000000000"
	// DefaultNVLinkLogicalPartitionId is the default NVLink Logical Partition ID for testing
	DefaultNVLinkLogicalPartitionId = "00000000-0000-4000-c000-000000000000"
	// DefaultTenantOrganizationId is the default TenantOrganization ID for testing
	DefaultTenantOrganizationId = "00000000-0000-4000-d000-000000000000"
	// DefaultSkuId is the default Sku ID for testing
	DefaultSkuId = "sku-dgx-h100-8x-default"
	// DefaultVpcPeeringId is the default VpcPeering ID for testing
	DefaultVpcPeeringId = "00000000-0000-4000-8001-000000000000"
	// DefaultPeerVpcId is the peer VPC ID used by the default VpcPeering
	DefaultPeerVpcId = "00000000-0000-4000-8002-000000000000"
	// DefaultDpuExtensionServiceId is the default DpuExtensionService ID for testing
	DefaultDpuExtensionServiceId = "00000000-0000-4000-8003-000000000000"
	// DefaultNetworkSecurityGroupId is the default NetworkSecurityGroup ID for testing
	DefaultNetworkSecurityGroupId = "00000000-0000-4000-8004-000000000000"
	// DefaultVpcPrefixId is the default VpcPrefix ID for testing
	DefaultVpcPrefixId = "00000000-0000-4000-e000-000000000001"
	// DefaultVpcPrefixCIDR is the default CIDR for the seeded VpcPrefix
	DefaultVpcPrefixCIDR = "10.0.0.0/16"
)

// NICoServerImpl implements interface NICoServer.
//
// The in-memory maps below have no per-field locking; every RPC handler is
// serialized by the unary interceptor registered in NICoTest, which takes
// mu for the duration of the call. Do not call handlers from other handlers
// without releasing the lock first.
type NICoServerImpl struct {
	cwssaws.UnimplementedForgeServer
	mu   sync.Mutex
	v    map[string]*cwssaws.Vpc
	ns   map[string]*cwssaws.NetworkSegment
	ins  map[string]*cwssaws.Instance
	m    map[string]*cwssaws.Machine
	tk   map[string]*cwssaws.TenantKeyset
	ibp  map[string]*cwssaws.IBPartition
	nvlp map[string]*cwssaws.NVLinkLogicalPartition
	em   map[string]*cwssaws.ExpectedMachine
	eps  map[string]*cwssaws.ExpectedPowerShelf
	es   map[string]*cwssaws.ExpectedSwitch
	er   map[string]*cwssaws.ExpectedRack
	tt   map[string]*cwssaws.Tenant
	vp   map[string]*cwssaws.VpcPrefix
	osi  map[string]*cwssaws.OsImage
	oss  map[string]*cwssaws.OperatingSystem
	it   map[string]*cwssaws.InstanceType
	nsg  map[string]*cwssaws.NetworkSecurityGroup

	// Per-org machine identity state.
	identityState    map[string]*identityOrgState
	tokenDelegations map[string]*cwssaws.TokenDelegationResponse

	// linkedExpectedMachines is populated from YAML when machine_id links are provided.
	linkedExpectedMachines []*cwssaws.LinkedExpectedMachine

	powerChecker libvirtfilter.PowerChecker
	provisioner  *libvirtfilter.Provisioner
	stateFile    string
}

// identityKeyMaterial is a per-org ES256 keypair plus its derived kid.
type identityKeyMaterial struct {
	privateKey *ecdsa.PrivateKey
	publicPEM  string
	kid        string
}

type identityOrgState struct {
	cfg                  *cwssaws.TenantIdentityConfigResponse
	slot1                *identityKeyMaterial
	slot2                *identityKeyMaterial
	currentSlot          int
	nonActiveSlotExpires *time.Time
}

func (s *identityOrgState) inactiveSlot() int {
	if s.currentSlot == 1 {
		return 2
	}
	return 1
}

func (s *identityOrgState) activeKey() *identityKeyMaterial {
	if s.currentSlot == 1 {
		return s.slot1
	}
	return s.slot2
}

func (s *identityOrgState) inactiveKey() *identityKeyMaterial {
	if s.currentSlot == 1 {
		return s.slot2
	}
	return s.slot1
}

func (s *identityOrgState) setSlot(n int, km *identityKeyMaterial) {
	if n == 1 {
		s.slot1 = km
	} else {
		s.slot2 = km
	}
}

func (s *identityOrgState) clearInactiveSlot() {
	if s.currentSlot == 1 {
		s.slot2 = nil
	} else {
		s.slot1 = nil
	}
	s.nonActiveSlotExpires = nil
}

func (f *NICoServerImpl) gcExpiredNonActiveSigningKey(orgID string) {
	st, ok := f.identityState[orgID]
	if !ok || st.nonActiveSlotExpires == nil {
		return
	}
	if !time.Now().Before(*st.nonActiveSlotExpires) {
		st.clearInactiveSlot()
	}
}

func tenantIdentitySigningKeysResponse(st *identityOrgState) []*cwssaws.TenantIdentitySigningKey {
	if st == nil {
		return nil
	}
	var entries []*cwssaws.TenantIdentitySigningKey
	if active := st.activeKey(); active != nil {
		entries = append(entries, &cwssaws.TenantIdentitySigningKey{
			Kid:           active.kid,
			Alg:           "ES256",
			CurrentSigner: true,
		})
	}
	if inactive := st.inactiveKey(); inactive != nil {
		entry := &cwssaws.TenantIdentitySigningKey{
			Kid:           inactive.kid,
			Alg:           "ES256",
			CurrentSigner: false,
		}
		if st.nonActiveSlotExpires != nil {
			entry.ExpireAt = timestamppb.New(*st.nonActiveSlotExpires)
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].GetKid() < entries[j].GetKid()
	})
	return entries
}

var logger = log.With().Str("component", "nico-core-mock").Logger()

// NewFromInventory creates a mock Forge server seeded from YAML inventory.
func NewFromInventory(inv *config.Inventory, powerChecker libvirtfilter.PowerChecker, provisioner *libvirtfilter.Provisioner) *NICoServerImpl {
	if powerChecker == nil {
		powerChecker = libvirtfilter.NoopChecker{}
	}
	srv := &NICoServerImpl{
		v:                make(map[string]*cwssaws.Vpc),
		ns:               make(map[string]*cwssaws.NetworkSegment),
		ins:              make(map[string]*cwssaws.Instance),
		m:                make(map[string]*cwssaws.Machine),
		tk:               make(map[string]*cwssaws.TenantKeyset),
		ibp:              make(map[string]*cwssaws.IBPartition),
		nvlp:             make(map[string]*cwssaws.NVLinkLogicalPartition),
		em:               make(map[string]*cwssaws.ExpectedMachine),
		eps:              make(map[string]*cwssaws.ExpectedPowerShelf),
		es:               make(map[string]*cwssaws.ExpectedSwitch),
		er:               make(map[string]*cwssaws.ExpectedRack),
		tt:               make(map[string]*cwssaws.Tenant),
		vp:               make(map[string]*cwssaws.VpcPrefix),
		osi:              make(map[string]*cwssaws.OsImage),
		oss:              make(map[string]*cwssaws.OperatingSystem),
		it:               make(map[string]*cwssaws.InstanceType),
		nsg:              make(map[string]*cwssaws.NetworkSecurityGroup),
		identityState:    make(map[string]*identityOrgState),
		tokenDelegations: make(map[string]*cwssaws.TokenDelegationResponse),
		powerChecker:     powerChecker,
		provisioner:      provisioner,
	}
	srv.loadFromInventory(inv)
	srv.LoadTestIdentity()
	return srv
}

func (f *NICoServerImpl) machineVisible(id string) bool {
	return f.powerChecker.IsPoweredOn(id)
}

func (f *NICoServerImpl) osImageLookupID(config *cwssaws.InstanceConfig) string {
	if config == nil || config.Os == nil {
		return ""
	}
	if id := config.Os.GetOsImageId(); id != nil && id.Value != "" {
		return id.Value
	}
	if id := config.Os.GetOperatingSystemId(); id != nil && id.Value != "" {
		return id.Value
	}
	return ""
}

func (f *NICoServerImpl) osImageSource(config *cwssaws.InstanceConfig) (url, digest string, capacity uint64, err error) {
	lookupID := f.osImageLookupID(config)
	if lookupID == "" {
		return "", "", 0, fmt.Errorf("instance config has no os_image_id")
	}
	img, ok := f.osi[lookupID]
	if !ok {
		return "", "", 0, fmt.Errorf("os image %q not found", lookupID)
	}
	attrs := img.GetAttributes()
	if attrs == nil || strings.TrimSpace(attrs.GetSourceUrl()) == "" {
		return "", "", 0, fmt.Errorf("os image %q has no source_url", lookupID)
	}
	return attrs.GetSourceUrl(), attrs.GetDigest(), attrs.GetCapacity(), nil
}

func (f *NICoServerImpl) scheduleLibvirtProvision(req *cwssaws.InstanceAllocationRequest) {
	if f.provisioner == nil || !f.provisioner.Enabled() {
		return
	}
	if req == nil || req.MachineId == nil {
		return
	}

	imageURL, imageDigest, capacity, err := f.osImageSource(req.Config)
	if err != nil {
		logger.Debug().
			Err(err).
			Str("machine_id", req.MachineId.GetId()).
			Msg("libvirt provisioning without os image")
	}

	machineID := req.MachineId.GetId()
	instanceID := machineID
	if req.InstanceId != nil && req.InstanceId.Value != "" {
		instanceID = req.InstanceId.Value
	}
	userData := f.resolveUserData(req.Config)
	instanceName := ""
	if req.Metadata != nil {
		instanceName = strings.TrimSpace(req.Metadata.GetName())
	}

	go func() {
		ctx := context.Background()
		if err := f.provisioner.ProvisionMachine(ctx, libvirtfilter.ProvisionRequest{
			MachineID:          machineID,
			InstanceID:         instanceID,
			InstanceName:       instanceName,
			ImageURL:           imageURL,
			ImageDigest:        imageDigest,
			ImageCapacityBytes: capacity,
			UserData:           userData,
		}); err != nil {
			logger.Error().
				Err(err).
				Str("machine_id", machineID).
				Str("image_url", imageURL).
				Msg("libvirt provisioning failed")
			return
		}
		if imageURL == "" {
			logger.Info().Str("machine_id", machineID).Msg("libvirt domain started without os image")
			return
		}
		logger.Info().Str("machine_id", machineID).Msg("libvirt provisioning succeeded")
	}()
}

func (f *NICoServerImpl) scheduleLibvirtRelease(instance *cwssaws.Instance) {
	if f.provisioner == nil || !f.provisioner.Enabled() {
		return
	}
	if instance == nil || instance.MachineId == nil {
		return
	}

	machineID := instance.MachineId.GetId()
	go func() {
		if err := f.provisioner.ReleaseMachine(context.Background(), machineID); err != nil {
			logger.Error().
				Err(err).
				Str("machine_id", machineID).
				Msg("libvirt release failed")
		}
	}()
}

func (f *NICoServerImpl) visibleMachineIDs() []string {
	ids := make([]string, 0, len(f.m))
	for id := range f.m {
		if f.machineVisible(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// Version implements interface NICoServer
func (f *NICoServerImpl) Version(ctx context.Context, req *cwssaws.VersionRequest) (*cwssaws.BuildInfo, error) {
	return &cwssaws.BuildInfo{
		BuildVersion: "nico-core-mock/1.0.0",
	}, nil
}

// CreateVpc implements interface NICoServer
func (f *NICoServerImpl) CreateVpc(c context.Context, req *cwssaws.VpcCreationRequest) (*cwssaws.Vpc, error) {
	if req == nil || req.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	// Honor the caller-supplied ID so the DB's ControllerVpcID matches what
	// inventory discovery later reports back; otherwise the VPC is treated
	// as missing on Site and flipped to Error.
	var nid string
	switch {
	case req.Id != nil && req.Id.Value != "":
		nid = req.Id.Value
	case f.v[DefaultVpcId] == nil:
		nid = DefaultVpcId
	default:
		nid = uuid.NewString()
	}

	if _, exists := f.v[nid]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "VPC with ID %q already exists", nid)
	}

	nv := &cwssaws.Vpc{
		Id:                        &cwssaws.VpcId{Value: nid},
		Name:                      req.Name,
		TenantOrganizationId:      req.TenantOrganizationId,
		NetworkVirtualizationType: req.NetworkVirtualizationType,
		RoutingProfileType:        req.RoutingProfileType,
		NetworkSecurityGroupId:    req.NetworkSecurityGroupId,
	}
	if req.Vni != nil {
		nv.Status = &cwssaws.VpcStatus{Vni: req.Vni}
	}
	f.v[nid] = nv

	return nv, nil
}

// UpdateVpc implements interface NICoServer
func (f *NICoServerImpl) UpdateVpc(c context.Context, req *cwssaws.VpcUpdateRequest) (*cwssaws.VpcUpdateResult, error) {
	if req == nil || req.Id == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	nv, ok := f.v[req.Id.Value]
	if ok {
		if req.Name != "" {
			nv.Name = req.Name
		}
		return &cwssaws.VpcUpdateResult{}, nil
	}

	return nil, status.Errorf(codes.NotFound, "VPC with ID %q not found", req.Id.Value)
}

// DeleteVpc implements interface NICoServer
func (f *NICoServerImpl) DeleteVpc(c context.Context, req *cwssaws.VpcDeletionRequest) (*cwssaws.VpcDeletionResult, error) {
	if req == nil || req.Id == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	_, ok := f.v[req.Id.Value]
	if ok {
		delete(f.v, req.Id.Value)
		return &cwssaws.VpcDeletionResult{}, nil
	}

	return nil, status.Errorf(codes.NotFound, "VPC with ID %q not found", req.Id.Value)
}

// FindVpcIds implements interface NICoServer
func (f *NICoServerImpl) FindVpcIds(ctx context.Context, req *cwssaws.VpcSearchFilter) (*cwssaws.VpcIdList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.VpcIdList{}
	for id := range f.v {
		response.VpcIds = append(response.VpcIds, &cwssaws.VpcId{Value: id})
	}
	return &response, nil
}

// FindVpcsByIds implements interface NICoServer
func (f *NICoServerImpl) FindVpcsByIds(ctx context.Context, req *cwssaws.VpcsByIdsRequest) (*cwssaws.VpcList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.VpcList{}
	for _, id := range req.VpcIds {
		if obj, ok := f.v[id.GetValue()]; ok {
			response.Vpcs = append(response.Vpcs, obj)
		}
	}
	return &response, nil
}

// FindVpcs implements interface NICoServer
func (f *NICoServerImpl) FindVpcs(c context.Context, req *cwssaws.VpcSearchQuery) (*cwssaws.VpcList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	res := []*cwssaws.Vpc{}

	for _, v := range f.v {
		res = append(res, v)
	}

	if req.Id != nil && req.Id.Value != "" {
		v, ok := f.v[req.Id.Value]
		if ok {
			res = []*cwssaws.Vpc{v}
		} else {
			res = []*cwssaws.Vpc{}
		}
	}

	if req.Name != nil {
		filtered := []*cwssaws.Vpc{}
		for _, nv := range f.v {
			if nv.Name == *req.Name {
				filtered = append(filtered, nv)
			}
		}
		res = filtered
	}

	return &cwssaws.VpcList{Vpcs: res}, nil
}

// CreateNetworkSegment implements interface NICoServer
func (f *NICoServerImpl) CreateNetworkSegment(c context.Context, req *cwssaws.NetworkSegmentCreationRequest) (*cwssaws.NetworkSegment, error) {
	if req == nil || req.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	nid := DefaultNetworkSegmentId
	_, ok := f.ns[DefaultNetworkSegmentId]
	if ok {
		// Default Network Segment already exists, create a new one with a different ID
		nid = uuid.NewString()
	}

	nns := &cwssaws.NetworkSegment{
		Id:          &cwssaws.NetworkSegmentId{Value: nid},
		Name:        req.Name,
		VpcId:       req.VpcId,
		SubdomainId: req.SubdomainId,
		Mtu:         req.Mtu,
		Prefixes:    req.Prefixes,
	}
	f.ns[nid] = nns

	return nns, nil
}

// DeleteNetworkSegment implements interface NICoServer
func (f *NICoServerImpl) DeleteNetworkSegment(c context.Context, req *cwssaws.NetworkSegmentDeletionRequest) (*cwssaws.NetworkSegmentDeletionResult, error) {
	if req == nil || req.Id == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	_, ok := f.ns[req.Id.Value]

	if ok {
		delete(f.ns, req.Id.Value)
		return &cwssaws.NetworkSegmentDeletionResult{}, nil
	}

	return nil, status.Errorf(codes.NotFound, "NetworkSegment with ID %q not found", req.Id.Value)
}

// FindNetworkSegmentIds implements interface NICoServer
func (f *NICoServerImpl) FindNetworkSegmentIds(ctx context.Context, req *cwssaws.NetworkSegmentSearchFilter) (*cwssaws.NetworkSegmentIdList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.NetworkSegmentIdList{}
	for id := range f.ns {
		response.NetworkSegmentsIds = append(response.NetworkSegmentsIds, &cwssaws.NetworkSegmentId{Value: id})
	}
	return &response, nil
}

// FindNetworkSegmentsByIds implements interface NICoServer
func (f *NICoServerImpl) FindNetworkSegmentsByIds(ctx context.Context, req *cwssaws.NetworkSegmentsByIdsRequest) (*cwssaws.NetworkSegmentList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.NetworkSegmentList{}
	for _, id := range req.NetworkSegmentsIds {
		if obj, ok := f.ns[id.GetValue()]; ok {
			response.NetworkSegments = append(response.NetworkSegments, obj)
		}
	}
	return &response, nil
}

// CreateInstance implements interface NICoServer
func (f *NICoServerImpl) AllocateInstance(ctx context.Context, req *cwssaws.InstanceAllocationRequest) (*cwssaws.Instance, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	nid := uuid.NewString()
	if req.InstanceId != nil {
		nid = req.InstanceId.Value
	}

	_, ok := f.ins[nid]
	if !ok {
		ifcsts := []*cwssaws.InstanceInterfaceStatus{}
		for _, ifcreq := range req.Config.Network.Interfaces {
			addr := ifcreq.GetIpAddress()
			if addr == "" {
				if pid := ifcreq.GetVpcPrefixId(); pid != nil && pid.Value != "" {
					if p, ok := f.vp[pid.Value]; ok && p.Config != nil && p.Config.Prefix != "" {
						if a, err := generateIPAddressInCIDR(p.Config.Prefix); err == nil {
							addr = a
						}
					}
				}
			}
			if addr == "" {
				addr = generateIPAddress()
			}
			ifcst := &cwssaws.InstanceInterfaceStatus{
				MacAddress: getStrPtr(generateMacAddress()),
				Addresses:  []string{addr},
			}
			if ifcreq.FunctionType == cwssaws.InterfaceFunctionType_VIRTUAL_FUNCTION {
				vfid := uint32(generateInteger(16))
				ifcst.VirtualFunctionId = &vfid
			}
			ifcsts = append(ifcsts, ifcst)
		}

		nins := cwssaws.Instance{
			Id:        &cwssaws.InstanceId{Value: nid},
			MachineId: req.MachineId,
			Config:    req.Config,
			Status: &cwssaws.InstanceStatus{
				Tenant: &cwssaws.InstanceTenantStatus{
					State: cwssaws.TenantState_PROVISIONING,
				},
				Network: &cwssaws.InstanceNetworkStatus{
					Interfaces: ifcsts,
				},
			},
		}

		f.ins[nid] = &nins

		m := cwssaws.Machine{
			Id:            req.MachineId,
			State:         "Ready",
			DiscoveryInfo: mockdata.MachineDiscoveryInfoForHost(mockdata.HostIDFromMachineID(req.MachineId.GetId())),
		}

		_, ok := f.m[req.MachineId.Id]
		if !ok {
			f.m[req.MachineId.Id] = &m
		} else {
			mockdata.EnsureMachineDiscoveryInfo(f.m[req.MachineId.Id])
		}

		f.scheduleLibvirtProvision(req)

		return &nins, nil
	}

	return nil, status.Errorf(codes.Internal, "Failed to create Instance")
}

// DeleteInstance implements interface NICoServer
func (f *NICoServerImpl) ReleaseInstance(c context.Context, req *cwssaws.InstanceReleaseRequest) (*cwssaws.InstanceReleaseResult, error) {
	if req == nil || req.Id == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	_, ok := f.ins[req.Id.Value]
	if ok {
		instance := f.ins[req.Id.Value]
		delete(f.ins, req.Id.Value)
		f.scheduleLibvirtRelease(instance)
		return &cwssaws.InstanceReleaseResult{}, nil
	}

	return nil, status.Errorf(codes.NotFound, "Instance with ID %q not found", req.Id.Value)
}

// FindInstances implements interface NICoServer
func (f *NICoServerImpl) FindInstanceIds(ctx context.Context, req *cwssaws.InstanceSearchFilter) (*cwssaws.InstanceIdList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.InstanceIdList{}
	for id := range f.ins {
		response.InstanceIds = append(response.InstanceIds, &cwssaws.InstanceId{Value: id})
	}
	return &response, nil
}

// FindInstances implements interface NICoServer.
//
// The site-agent's inventory discovery cron is the only consumer that observes
// instance state, so we advance the mocked lifecycle one step per read here
// (PROVISIONING → CONFIGURING → READY). Real Core would advance state from
// machine-controller signals; this mock has no machine-controller, so without
// this nudge instances stay in PROVISIONING forever.
func (f *NICoServerImpl) FindInstancesByIds(ctx context.Context, req *cwssaws.InstancesByIdsRequest) (*cwssaws.InstanceList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.InstanceList{}
	for _, id := range req.InstanceIds {
		if obj, ok := f.ins[id.GetValue()]; ok {
			if obj.Status != nil && obj.Status.Tenant != nil {
				switch obj.Status.Tenant.State {
				case cwssaws.TenantState_PROVISIONING:
					obj.Status.Tenant.State = cwssaws.TenantState_CONFIGURING
				case cwssaws.TenantState_CONFIGURING:
					obj.Status.Tenant.State = cwssaws.TenantState_READY
				}
			}
			response.Instances = append(response.Instances, obj)
		}
	}
	return &response, nil
}

// InvokeInstancePower implements interface NICoServer
func (f *NICoServerImpl) InvokeInstancePower(c context.Context, req *cwssaws.InstancePowerRequest) (*cwssaws.InstancePowerResult, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	_, ok := f.m[req.MachineId.Id]
	if ok && f.machineVisible(req.MachineId.Id) {
		if req.Operation == cwssaws.InstancePowerRequest_POWER_RESET {
			return &cwssaws.InstancePowerResult{}, nil
		}

		return &cwssaws.InstancePowerResult{}, status.Errorf(codes.InvalidArgument, "Invalid operation in request")
	}

	return nil, status.Errorf(codes.NotFound, "Machine with ID %q not found", req.MachineId.Id)
}

func (f *NICoServerImpl) FindMachineIds(ctx context.Context, req *cwssaws.MachineSearchConfig) (*cwssaws.MachineIdList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.MachineIdList{}
	for _, id := range f.visibleMachineIDs() {
		response.MachineIds = append(response.MachineIds, &cwssaws.MachineId{Id: id})
	}

	return &response, nil
}

// FindMachinesByIds implements interface NICoServer
func (f *NICoServerImpl) FindMachinesByIds(ctx context.Context, req *cwssaws.MachinesByIdsRequest) (*cwssaws.MachineList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.MachineList{}
	for _, id := range req.MachineIds {
		if obj, ok := f.m[id.GetId()]; ok && f.machineVisible(id.GetId()) {
			mockdata.EnsureMachineDiscoveryInfo(obj)
			response.Machines = append(response.Machines, obj)
		}
	}
	return &response, nil
}

// The following stubs serve the inventory Discover* workflows in local dev.
// The site-agent calls these on its 3-minute inventory cron; they return
// realistic fake data drawn from the in-memory state (or static defaults
// where there is no backing map) so downstream workflows have something to
// chew on instead of always seeing empty lists.
func (f *NICoServerImpl) GetAllExpectedMachines(ctx context.Context, req *emptypb.Empty) (*cwssaws.ExpectedMachineList, error) {
	res := make([]*cwssaws.ExpectedMachine, 0, len(f.em))
	for _, em := range f.em {
		res = append(res, em)
	}
	return &cwssaws.ExpectedMachineList{ExpectedMachines: res}, nil
}

func (f *NICoServerImpl) FindInstanceTypeIds(ctx context.Context, req *cwssaws.FindInstanceTypeIdsRequest) (*cwssaws.FindInstanceTypeIdsResponse, error) {
	ids := make([]string, 0, len(f.it))
	for id := range f.it {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return &cwssaws.FindInstanceTypeIdsResponse{InstanceTypeIds: ids}, nil
}

func (f *NICoServerImpl) AssociateMachinesWithInstanceType(ctx context.Context, req *cwssaws.AssociateMachinesWithInstanceTypeRequest) (*cwssaws.AssociateMachinesWithInstanceTypeResponse, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	if req.InstanceTypeId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "instance_type_id is required")
	}
	if _, ok := f.it[req.InstanceTypeId]; !ok {
		return nil, status.Errorf(codes.NotFound, "InstanceType with ID %q not found", req.InstanceTypeId)
	}
	if len(req.MachineIds) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "machine_ids is required")
	}
	return &cwssaws.AssociateMachinesWithInstanceTypeResponse{}, nil
}

func (f *NICoServerImpl) RemoveMachineInstanceTypeAssociation(ctx context.Context, req *cwssaws.RemoveMachineInstanceTypeAssociationRequest) (*cwssaws.RemoveMachineInstanceTypeAssociationResponse, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	if req.MachineId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "machine_id is required")
	}
	return &cwssaws.RemoveMachineInstanceTypeAssociationResponse{}, nil
}

func (f *NICoServerImpl) FindNVLinkLogicalPartitionIds(ctx context.Context, req *cwssaws.NVLinkLogicalPartitionSearchFilter) (*cwssaws.NVLinkLogicalPartitionIdList, error) {
	response := cwssaws.NVLinkLogicalPartitionIdList{}
	for id := range f.nvlp {
		response.PartitionIds = append(response.PartitionIds, &cwssaws.NVLinkLogicalPartitionId{Value: id})
	}
	return &response, nil
}

func (f *NICoServerImpl) FindNVLinkLogicalPartitionsByIds(ctx context.Context, req *cwssaws.NVLinkLogicalPartitionsByIdsRequest) (*cwssaws.NVLinkLogicalPartitionList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.NVLinkLogicalPartitionList{}
	for _, id := range req.PartitionIds {
		if obj, ok := f.nvlp[id.GetValue()]; ok {
			response.Partitions = append(response.Partitions, obj)
		}
	}
	return &response, nil
}

// CreateNVLinkLogicalPartition implements interface NICoServer
func (f *NICoServerImpl) CreateNVLinkLogicalPartition(ctx context.Context, req *cwssaws.NVLinkLogicalPartitionCreationRequest) (*cwssaws.NVLinkLogicalPartition, error) {
	if req == nil || req.Config == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	nid := DefaultNVLinkLogicalPartitionId
	if req.Id != nil && req.Id.Value != "" {
		nid = req.Id.Value
	} else if _, ok := f.nvlp[DefaultNVLinkLogicalPartitionId]; ok {
		// Default NVLink Logical Partition already exists, create a new one with a different ID
		nid = uuid.NewString()
	}

	nvlp := &cwssaws.NVLinkLogicalPartition{
		Id: &cwssaws.NVLinkLogicalPartitionId{Value: nid},
		Config: &cwssaws.NVLinkLogicalPartitionConfig{
			TenantOrganizationId: req.Config.TenantOrganizationId,
			Metadata:             req.Config.Metadata,
		},
		Status: &cwssaws.NVLinkLogicalPartitionStatus{State: cwssaws.TenantState_READY},
	}
	f.nvlp[nid] = nvlp
	return nvlp, nil
}

// UpdateNVLinkLogicalPartition implements interface NICoServer
func (f *NICoServerImpl) UpdateNVLinkLogicalPartition(ctx context.Context, req *cwssaws.NVLinkLogicalPartitionUpdateRequest) (*cwssaws.NVLinkLogicalPartitionUpdateResult, error) {
	if req == nil || req.Id == nil || req.Id.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	nvlp, ok := f.nvlp[req.Id.Value]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "NVLink Logical Partition with ID %q not found", req.Id.Value)
	}
	if req.Config != nil {
		if nvlp.Config == nil {
			nvlp.Config = &cwssaws.NVLinkLogicalPartitionConfig{}
		}
		if req.Config.TenantOrganizationId != "" {
			nvlp.Config.TenantOrganizationId = req.Config.TenantOrganizationId
		}
		if req.Config.Metadata != nil {
			nvlp.Config.Metadata = req.Config.Metadata
		}
	}
	return &cwssaws.NVLinkLogicalPartitionUpdateResult{}, nil
}

// DeleteNVLinkLogicalPartition implements interface NICoServer
func (f *NICoServerImpl) DeleteNVLinkLogicalPartition(ctx context.Context, req *cwssaws.NVLinkLogicalPartitionDeletionRequest) (*cwssaws.NVLinkLogicalPartitionDeletionResult, error) {
	if req == nil || req.Id == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	if _, ok := f.nvlp[req.Id.Value]; ok {
		delete(f.nvlp, req.Id.Value)
		return &cwssaws.NVLinkLogicalPartitionDeletionResult{}, nil
	}
	return nil, status.Errorf(codes.NotFound, "NVLink Logical Partition with ID %q not found", req.Id.Value)
}

// NVLinkLogicalPartitionsForTenant implements interface NICoServer
func (f *NICoServerImpl) NVLinkLogicalPartitionsForTenant(ctx context.Context, req *cwssaws.TenantSearchQuery) (*cwssaws.NVLinkLogicalPartitionList, error) {
	response := cwssaws.NVLinkLogicalPartitionList{}
	for _, nvlp := range f.nvlp {
		if req.GetTenantOrganizationId() == "" || (nvlp.Config != nil && nvlp.Config.TenantOrganizationId == req.GetTenantOrganizationId()) {
			response.Partitions = append(response.Partitions, nvlp)
		}
	}
	return &response, nil
}

func (f *NICoServerImpl) FindTenantOrganizationIds(ctx context.Context, req *cwssaws.TenantSearchFilter) (*cwssaws.TenantOrganizationIdList, error) {
	ids := []string{"00000000-0000-4000-d000-000000000000"}
	for id := range f.tt {
		if id == "00000000-0000-4000-d000-000000000000" {
			continue
		}
		ids = append(ids, id)
	}
	return &cwssaws.TenantOrganizationIdList{TenantOrganizationIds: ids}, nil
}

func (f *NICoServerImpl) FindTenantsByOrganizationIds(ctx context.Context, req *cwssaws.TenantByOrganizationIdsRequest) (*cwssaws.TenantList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	resp := &cwssaws.TenantList{}
	for _, id := range req.OrganizationIds {
		if t, ok := f.tt[id]; ok {
			resp.Tenants = append(resp.Tenants, t)
			continue
		}
		// Synthesize a Tenant for the well-known default so DiscoverTenantInventory
		// has something to publish even before any explicit CreateTenant call.
		resp.Tenants = append(resp.Tenants, &cwssaws.Tenant{OrganizationId: id})
	}
	return resp, nil
}

func (f *NICoServerImpl) CreateTenant(ctx context.Context, req *cwssaws.CreateTenantRequest) (*cwssaws.CreateTenantResponse, error) {
	if req == nil || req.OrganizationId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	t := &cwssaws.Tenant{
		OrganizationId:     req.OrganizationId,
		Metadata:           req.Metadata,
		RoutingProfileType: req.RoutingProfileType,
	}
	f.tt[req.OrganizationId] = t
	return &cwssaws.CreateTenantResponse{Tenant: t}, nil
}

func (f *NICoServerImpl) UpdateTenant(ctx context.Context, req *cwssaws.UpdateTenantRequest) (*cwssaws.UpdateTenantResponse, error) {
	if req == nil || req.OrganizationId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	t, ok := f.tt[req.OrganizationId]
	if !ok {
		t = &cwssaws.Tenant{OrganizationId: req.OrganizationId}
		f.tt[req.OrganizationId] = t
	}
	if req.Metadata != nil {
		t.Metadata = req.Metadata
	}
	if req.RoutingProfileType != nil {
		t.RoutingProfileType = req.RoutingProfileType
	}
	return &cwssaws.UpdateTenantResponse{Tenant: t}, nil
}

// The Find{Sku,VpcPeering,DpuExtensionService,NetworkSecurityGroup}Ids /
// Find*ByIds pairs back the corresponding inventory Discover workflows.
// We synthesize a small set of plausible fake entities so downstream Cloud
// workflows see a non-empty inventory page instead of hitting
// "no fallback find function defined" on the Unimplemented default.

func (f *NICoServerImpl) GetAllSkuIds(ctx context.Context, req *emptypb.Empty) (*cwssaws.SkuIdList, error) {
	return &cwssaws.SkuIdList{Ids: []string{DefaultSkuId}}, nil
}

func (f *NICoServerImpl) FindSkusByIds(ctx context.Context, req *cwssaws.SkusByIdsRequest) (*cwssaws.SkuList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	ids := req.Ids
	if len(ids) == 0 {
		ids = []string{DefaultSkuId}
	}
	res := make([]*cwssaws.Sku, 0, len(ids))
	for _, id := range ids {
		// Associate the SKU with whatever Machine LoadTestMachines created, so
		// Sku → Machine references are internally consistent in the mock.
		var assoc []*cwssaws.MachineId
		for _, mid := range f.visibleMachineIDs() {
			assoc = append(assoc, &cwssaws.MachineId{Id: mid})
		}
		desc := "Mock SKU describing a DGX H100 8x reference platform"
		deviceType := "dgx-h100-8x"
		res = append(res, &cwssaws.Sku{
			Id:                   id,
			Description:          &desc,
			Created:              timestamppb.Now(),
			SchemaVersion:        1,
			AssociatedMachineIds: assoc,
			DeviceType:           &deviceType,
		})
	}
	return &cwssaws.SkuList{Skus: res}, nil
}

func (f *NICoServerImpl) FindVpcPeeringIds(ctx context.Context, req *cwssaws.VpcPeeringSearchFilter) (*cwssaws.VpcPeeringIdList, error) {
	return &cwssaws.VpcPeeringIdList{
		VpcPeeringIds: []*cwssaws.VpcPeeringId{{Value: DefaultVpcPeeringId}},
	}, nil
}

func (f *NICoServerImpl) FindVpcPeeringsByIds(ctx context.Context, req *cwssaws.VpcPeeringsByIdsRequest) (*cwssaws.VpcPeeringList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	ids := req.VpcPeeringIds
	if len(ids) == 0 {
		ids = []*cwssaws.VpcPeeringId{{Value: DefaultVpcPeeringId}}
	}
	res := make([]*cwssaws.VpcPeering, 0, len(ids))
	for _, id := range ids {
		res = append(res, &cwssaws.VpcPeering{
			Id:        id,
			VpcId:     &cwssaws.VpcId{Value: DefaultVpcId},
			PeerVpcId: &cwssaws.VpcId{Value: DefaultPeerVpcId},
		})
	}
	return &cwssaws.VpcPeeringList{VpcPeerings: res}, nil
}

func (f *NICoServerImpl) FindDpuExtensionServiceIds(ctx context.Context, req *cwssaws.DpuExtensionServiceSearchFilter) (*cwssaws.DpuExtensionServiceIdList, error) {
	return &cwssaws.DpuExtensionServiceIdList{ServiceIds: []string{DefaultDpuExtensionServiceId}}, nil
}

func (f *NICoServerImpl) FindDpuExtensionServicesByIds(ctx context.Context, req *cwssaws.DpuExtensionServicesByIdsRequest) (*cwssaws.DpuExtensionServiceList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	ids := req.ServiceIds
	if len(ids) == 0 {
		ids = []string{DefaultDpuExtensionServiceId}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res := make([]*cwssaws.DpuExtensionService, 0, len(ids))
	for _, id := range ids {
		res = append(res, &cwssaws.DpuExtensionService{
			ServiceId:            id,
			ServiceType:          cwssaws.DpuExtensionServiceType_KUBERNETES_POD,
			ServiceName:          "mock-dpu-extension-service",
			TenantOrganizationId: DefaultTenantOrganizationId,
			VersionCtr:           1,
			ActiveVersions:       []string{"v1"},
			Description:          "Mock DPU extension service for kind-reset",
			Created:              now,
			Updated:              now,
		})
	}
	return &cwssaws.DpuExtensionServiceList{Services: res}, nil
}

func (f *NICoServerImpl) CreateNetworkSecurityGroup(ctx context.Context, req *cwssaws.CreateNetworkSecurityGroupRequest) (*cwssaws.CreateNetworkSecurityGroupResponse, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	var nid string
	switch {
	case req.Id != nil && *req.Id != "":
		nid = *req.Id
	case f.nsg[DefaultNetworkSecurityGroupId] == nil:
		nid = DefaultNetworkSecurityGroupId
	default:
		nid = uuid.NewString()
	}

	if _, exists := f.nsg[nid]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "NetworkSecurityGroup with ID %q already exists", nid)
	}

	n := &cwssaws.NetworkSecurityGroup{
		Id:                   nid,
		TenantOrganizationId: req.TenantOrganizationId,
		Metadata:             req.Metadata,
		Version:              "1",
		Attributes:           req.NetworkSecurityGroupAttributes,
	}
	f.nsg[nid] = n

	return &cwssaws.CreateNetworkSecurityGroupResponse{NetworkSecurityGroup: n}, nil
}

func (f *NICoServerImpl) UpdateNetworkSecurityGroup(ctx context.Context, req *cwssaws.UpdateNetworkSecurityGroupRequest) (*cwssaws.UpdateNetworkSecurityGroupResponse, error) {
	if req == nil || req.Id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	n, ok := f.nsg[req.Id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "NetworkSecurityGroup with ID %q not found", req.Id)
	}

	if req.Metadata != nil {
		n.Metadata = req.Metadata
	}
	if req.NetworkSecurityGroupAttributes != nil {
		n.Attributes = req.NetworkSecurityGroupAttributes
	}
	// Bump version to signal an update happened.
	n.Version = fmt.Sprintf("%d", time.Now().UnixNano())

	return &cwssaws.UpdateNetworkSecurityGroupResponse{NetworkSecurityGroup: n}, nil
}

func (f *NICoServerImpl) DeleteNetworkSecurityGroup(ctx context.Context, req *cwssaws.DeleteNetworkSecurityGroupRequest) (*cwssaws.DeleteNetworkSecurityGroupResponse, error) {
	if req == nil || req.Id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	if _, ok := f.nsg[req.Id]; !ok {
		return nil, status.Errorf(codes.NotFound, "NetworkSecurityGroup with ID %q not found", req.Id)
	}
	delete(f.nsg, req.Id)

	return &cwssaws.DeleteNetworkSecurityGroupResponse{}, nil
}

func (f *NICoServerImpl) FindNetworkSecurityGroupIds(ctx context.Context, req *cwssaws.FindNetworkSecurityGroupIdsRequest) (*cwssaws.FindNetworkSecurityGroupIdsResponse, error) {
	ids := make([]string, 0, len(f.nsg)+1)
	if _, ok := f.nsg[DefaultNetworkSecurityGroupId]; !ok {
		ids = append(ids, DefaultNetworkSecurityGroupId)
	}
	for id := range f.nsg {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return &cwssaws.FindNetworkSecurityGroupIdsResponse{
		NetworkSecurityGroupIds: ids,
	}, nil
}

func (f *NICoServerImpl) FindNetworkSecurityGroupsByIds(ctx context.Context, req *cwssaws.FindNetworkSecurityGroupsByIdsRequest) (*cwssaws.FindNetworkSecurityGroupsByIdsResponse, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	ids := req.NetworkSecurityGroupIds
	if len(ids) == 0 {
		ids = make([]string, 0, len(f.nsg)+1)
		if _, ok := f.nsg[DefaultNetworkSecurityGroupId]; !ok {
			ids = append(ids, DefaultNetworkSecurityGroupId)
		}
		for id := range f.nsg {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	tenantID := DefaultTenantOrganizationId
	if req.TenantOrganizationId != nil && *req.TenantOrganizationId != "" {
		tenantID = *req.TenantOrganizationId
	}
	res := make([]*cwssaws.NetworkSecurityGroup, 0, len(ids))
	for _, id := range ids {
		if n, ok := f.nsg[id]; ok {
			res = append(res, n)
			continue
		}
		res = append(res, &cwssaws.NetworkSecurityGroup{
			Id:                   id,
			TenantOrganizationId: tenantID,
			Version:              "1",
		})
	}
	return &cwssaws.FindNetworkSecurityGroupsByIdsResponse{NetworkSecurityGroups: res}, nil
}

func newMockVpcPrefix(id, cidr, vpcId string) *cwssaws.VpcPrefix {
	return &cwssaws.VpcPrefix{
		Id:     &cwssaws.VpcPrefixId{Value: id},
		VpcId:  &cwssaws.VpcId{Value: vpcId},
		Config: &cwssaws.VpcPrefixConfig{Prefix: cidr},
		Status: &cwssaws.VpcPrefixStatus{
			Total_31Segments:         1024,
			Available_31Segments:     1024,
			TotalLinknetSegments:     256,
			AvailableLinknetSegments: 256,
		},
	}
}

func (f *NICoServerImpl) GetVpcPrefixes(ctx context.Context, req *cwssaws.VpcPrefixGetRequest) (*cwssaws.VpcPrefixList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	res := make([]*cwssaws.VpcPrefix, 0, len(req.VpcPrefixIds))
	if len(req.VpcPrefixIds) == 0 {
		// Return everything tracked, plus the seeded default so the inventory
		// page is never empty before any explicit CreateVpcPrefix call.
		seenDefault := false
		for id, p := range f.vp {
			res = append(res, p)
			if id == DefaultVpcPrefixId {
				seenDefault = true
			}
		}
		if !seenDefault {
			res = append(res, newMockVpcPrefix(DefaultVpcPrefixId, DefaultVpcPrefixCIDR, DefaultVpcId))
		}
		return &cwssaws.VpcPrefixList{VpcPrefixes: res}, nil
	}
	for _, id := range req.VpcPrefixIds {
		if p, ok := f.vp[id.GetValue()]; ok {
			res = append(res, p)
			continue
		}
		// Synthesize a prefix for any unknown ID so the inventory loop stays
		// consistent with whatever SearchVpcPrefixes reported.
		res = append(res, newMockVpcPrefix(id.GetValue(), DefaultVpcPrefixCIDR, DefaultVpcId))
	}
	return &cwssaws.VpcPrefixList{VpcPrefixes: res}, nil
}

// SearchVpcPrefixes implements interface NICoServer. The filters in
// VpcPrefixSearchQuery are ignored — the mock returns every tracked ID plus
// the seeded default.
func (f *NICoServerImpl) SearchVpcPrefixes(ctx context.Context, req *cwssaws.VpcPrefixSearchQuery) (*cwssaws.VpcPrefixIdList, error) {
	ids := make([]*cwssaws.VpcPrefixId, 0, len(f.vp)+1)
	seenDefault := false
	for id := range f.vp {
		ids = append(ids, &cwssaws.VpcPrefixId{Value: id})
		if id == DefaultVpcPrefixId {
			seenDefault = true
		}
	}
	if !seenDefault {
		ids = append(ids, &cwssaws.VpcPrefixId{Value: DefaultVpcPrefixId})
	}
	return &cwssaws.VpcPrefixIdList{VpcPrefixIds: ids}, nil
}

// CreateVpcPrefix implements interface NICoServer
func (f *NICoServerImpl) CreateVpcPrefix(ctx context.Context, req *cwssaws.VpcPrefixCreationRequest) (*cwssaws.VpcPrefix, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	var nid string
	switch {
	case req.Id != nil && req.Id.Value != "":
		nid = req.Id.Value
	case f.vp[DefaultVpcPrefixId] == nil:
		nid = DefaultVpcPrefixId
	default:
		nid = uuid.NewString()
	}
	if _, exists := f.vp[nid]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "VpcPrefix with ID %q already exists", nid)
	}
	cidr := DefaultVpcPrefixCIDR
	if req.Config != nil && req.Config.Prefix != "" {
		cidr = req.Config.Prefix
	} else if req.Prefix != "" {
		cidr = req.Prefix
	}
	vpcID := DefaultVpcId
	if req.VpcId != nil && req.VpcId.Value != "" {
		vpcID = req.VpcId.Value
	}
	p := newMockVpcPrefix(nid, cidr, vpcID)
	p.Name = req.Name
	p.Metadata = req.Metadata
	f.vp[nid] = p
	return p, nil
}

// UpdateVpcPrefix implements interface NICoServer
func (f *NICoServerImpl) UpdateVpcPrefix(ctx context.Context, req *cwssaws.VpcPrefixUpdateRequest) (*cwssaws.VpcPrefix, error) {
	if req == nil || req.Id == nil || req.Id.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	p, ok := f.vp[req.Id.Value]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "VpcPrefix with ID %q not found", req.Id.Value)
	}
	if req.Config != nil && req.Config.Prefix != "" {
		if p.Config == nil {
			p.Config = &cwssaws.VpcPrefixConfig{}
		}
		p.Config.Prefix = req.Config.Prefix
	} else if req.Prefix != nil && *req.Prefix != "" {
		if p.Config == nil {
			p.Config = &cwssaws.VpcPrefixConfig{}
		}
		p.Config.Prefix = *req.Prefix
	}
	if req.Name != nil {
		p.Name = *req.Name
	}
	if req.Metadata != nil {
		p.Metadata = req.Metadata
	}
	return p, nil
}

// DeleteVpcPrefix implements interface NICoServer
func (f *NICoServerImpl) DeleteVpcPrefix(ctx context.Context, req *cwssaws.VpcPrefixDeletionRequest) (*cwssaws.VpcPrefixDeletionResult, error) {
	if req == nil || req.Id == nil || req.Id.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	if _, ok := f.vp[req.Id.Value]; !ok {
		return nil, status.Errorf(codes.NotFound, "VpcPrefix with ID %q not found", req.Id.Value)
	}
	delete(f.vp, req.Id.Value)
	return &cwssaws.VpcPrefixDeletionResult{}, nil
}

func (f *NICoServerImpl) ListOsImage(ctx context.Context, req *cwssaws.ListOsImageRequest) (*cwssaws.ListOsImageResponse, error) {
	res := make([]*cwssaws.OsImage, 0, len(f.osi))
	for _, img := range f.osi {
		res = append(res, img)
	}
	return &cwssaws.ListOsImageResponse{Images: res}, nil
}

// CreateOsImage implements interface NICoServer. Stores the image with
// ImageReady status so the next inventory discovery cycle reports it as
// synced back up to the cloud DB.
func (f *NICoServerImpl) CreateOsImage(ctx context.Context, req *cwssaws.OsImageAttributes) (*cwssaws.OsImage, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	var nid string
	if req.Id != nil && req.Id.Value != "" {
		nid = req.Id.Value
	} else {
		nid = uuid.NewString()
		req.Id = &cwssaws.UUID{Value: nid}
	}
	if _, exists := f.osi[nid]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "OsImage with ID %q already exists", nid)
	}
	img := &cwssaws.OsImage{
		Attributes: req,
		Status:     cwssaws.OsImageStatus_ImageReady,
	}
	f.osi[nid] = img
	f.ensureOperatingSystemStub(nid, req)
	return img, nil
}

// UpdateOsImage implements interface NICoServer.
func (f *NICoServerImpl) UpdateOsImage(ctx context.Context, req *cwssaws.OsImageAttributes) (*cwssaws.OsImage, error) {
	if req == nil || req.Id == nil || req.Id.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	img, ok := f.osi[req.Id.Value]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "OsImage with ID %q not found", req.Id.Value)
	}
	img.Attributes = req
	f.ensureOperatingSystemStub(req.Id.Value, req)
	return img, nil
}

// DeleteOsImage implements interface NICoServer.
func (f *NICoServerImpl) DeleteOsImage(ctx context.Context, req *cwssaws.DeleteOsImageRequest) (*cwssaws.DeleteOsImageResponse, error) {
	if req == nil || req.Id == nil || req.Id.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	if _, ok := f.osi[req.Id.Value]; !ok {
		return nil, status.Errorf(codes.NotFound, "OsImage with ID %q not found", req.Id.Value)
	}
	delete(f.osi, req.Id.Value)
	delete(f.oss, req.Id.Value)
	return &cwssaws.DeleteOsImageResponse{}, nil
}

// GetOsImage implements interface NICoServer.
func (f *NICoServerImpl) GetOsImage(ctx context.Context, req *cwssaws.UUID) (*cwssaws.OsImage, error) {
	if req == nil || req.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	img, ok := f.osi[req.Value]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "OsImage with ID %q not found", req.Value)
	}
	return img, nil
}

func (f *NICoServerImpl) GetAllExpectedMachinesLinked(ctx context.Context, req *emptypb.Empty) (*cwssaws.LinkedExpectedMachineList, error) {
	if len(f.linkedExpectedMachines) > 0 {
		return &cwssaws.LinkedExpectedMachineList{
			ExpectedMachines: append([]*cwssaws.LinkedExpectedMachine(nil), f.linkedExpectedMachines...),
		}, nil
	}

	res := make([]*cwssaws.LinkedExpectedMachine, 0, len(f.em))
	for _, em := range f.em {
		linked := &cwssaws.LinkedExpectedMachine{
			ChassisSerialNumber: em.ChassisSerialNumber,
			BmcMacAddress:       em.BmcMacAddress,
			ExpectedMachineId:   em.Id,
		}
		for _, mid := range f.visibleMachineIDs() {
			linked.MachineId = &cwssaws.MachineId{Id: mid}
			break
		}
		res = append(res, linked)
	}
	return &cwssaws.LinkedExpectedMachineList{ExpectedMachines: res}, nil
}

func (f *NICoServerImpl) GetAllExpectedPowerShelvesLinked(ctx context.Context, req *emptypb.Empty) (*cwssaws.LinkedExpectedPowerShelfList, error) {
	res := make([]*cwssaws.LinkedExpectedPowerShelf, 0, len(f.eps))
	for _, eps := range f.eps {
		res = append(res, &cwssaws.LinkedExpectedPowerShelf{
			ShelfSerialNumber:    eps.ShelfSerialNumber,
			BmcMacAddress:        eps.BmcMacAddress,
			ExpectedPowerShelfId: eps.ExpectedPowerShelfId,
			PowerShelfId:         &cwssaws.PowerShelfId{Id: uuid.NewString()},
		})
	}
	return &cwssaws.LinkedExpectedPowerShelfList{ExpectedPowerShelves: res}, nil
}

func (f *NICoServerImpl) GetAllExpectedSwitchesLinked(ctx context.Context, req *emptypb.Empty) (*cwssaws.LinkedExpectedSwitchList, error) {
	res := make([]*cwssaws.LinkedExpectedSwitch, 0, len(f.es))
	for _, es := range f.es {
		res = append(res, &cwssaws.LinkedExpectedSwitch{
			SwitchSerialNumber: es.SwitchSerialNumber,
			BmcMacAddress:      es.BmcMacAddress,
			ExpectedSwitchId:   es.ExpectedSwitchId,
			SwitchId:           &cwssaws.SwitchId{Id: uuid.NewString()},
		})
	}
	return &cwssaws.LinkedExpectedSwitchList{ExpectedSwitches: res}, nil
}

func (f *NICoServerImpl) GetNetworkSecurityGroupPropagationStatus(ctx context.Context, req *cwssaws.GetNetworkSecurityGroupPropagationStatusRequest) (*cwssaws.GetNetworkSecurityGroupPropagationStatusResponse, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	resp := &cwssaws.GetNetworkSecurityGroupPropagationStatusResponse{}

	vpcIds := req.VpcIds
	if len(vpcIds) == 0 {
		for id := range f.v {
			vpcIds = append(vpcIds, id)
		}
	}
	for _, id := range vpcIds {
		resp.Vpcs = append(resp.Vpcs, &cwssaws.NetworkSecurityGroupPropagationObjectStatus{
			Id:     id,
			Status: cwssaws.NetworkSecurityGroupPropagationStatus_NSG_PROP_STATUS_FULL,
		})
	}

	instanceIds := req.InstanceIds
	if len(instanceIds) == 0 {
		for id := range f.ins {
			instanceIds = append(instanceIds, id)
		}
	}
	for _, id := range instanceIds {
		resp.Instances = append(resp.Instances, &cwssaws.NetworkSecurityGroupPropagationObjectStatus{
			Id:     id,
			Status: cwssaws.NetworkSecurityGroupPropagationStatus_NSG_PROP_STATUS_FULL,
		})
	}
	return resp, nil
}

func (f *NICoServerImpl) SetMaintenance(context.Context, *cwssaws.MaintenanceRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// CreateTenantKeyset implements interface NICoServer
func (f *NICoServerImpl) CreateTenantKeyset(c context.Context, req *cwssaws.CreateTenantKeysetRequest) (*cwssaws.CreateTenantKeysetResponse, error) {
	if req == nil || req.KeysetIdentifier == nil || req.KeysetIdentifier.KeysetId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	nid := DefaultTenantKeysetId
	_, ok := f.tk[DefaultTenantKeysetId]
	if ok {
		// Default TenantKeyset already exists, create a new one with a different ID
		nid = uuid.NewString()
	}

	ntk := &cwssaws.TenantKeyset{
		KeysetIdentifier: &cwssaws.TenantKeysetIdentifier{
			KeysetId: nid,
		},
		KeysetContent: req.KeysetContent,
		Version:       req.Version,
	}

	f.tk[nid] = ntk

	result := &cwssaws.CreateTenantKeysetResponse{
		Keyset: ntk,
	}

	return result, nil
}

// UpdateTenantKeyset implements interface NICoServer
func (f *NICoServerImpl) UpdateTenantKeyset(c context.Context, req *cwssaws.UpdateTenantKeysetRequest) (*cwssaws.UpdateTenantKeysetResponse, error) {
	if req == nil || req.KeysetIdentifier == nil || req.KeysetIdentifier.KeysetId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	eid := req.KeysetIdentifier.KeysetId

	_, ok := f.tk[eid]
	if ok {
		f.tk[eid].KeysetContent = req.KeysetContent
		f.tk[eid].Version = req.Version

		return &cwssaws.UpdateTenantKeysetResponse{}, nil
	}

	return nil, status.Errorf(codes.Internal, "TenantKeyset with ID not found")
}

// DeleteTenantKeyset implements interface NICoServer
func (f *NICoServerImpl) DeleteTenantKeyset(c context.Context, req *cwssaws.DeleteTenantKeysetRequest) (*cwssaws.DeleteTenantKeysetResponse, error) {
	if req == nil || req.KeysetIdentifier == nil || req.KeysetIdentifier.KeysetId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	eid := req.KeysetIdentifier.KeysetId

	_, ok := f.tk[eid]
	if ok {
		delete(f.tk, eid)
		return &cwssaws.DeleteTenantKeysetResponse{}, nil
	}

	return nil, status.Errorf(codes.NotFound, "TenantKeyset with ID %q not found", eid)
}

// FindTenantKeysetIds implements interface NICoServer
func (f *NICoServerImpl) FindTenantKeysetIds(ctx context.Context, req *cwssaws.TenantKeysetSearchFilter) (*cwssaws.TenantKeysetIdList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.TenantKeysetIdList{}
	for id := range f.tk {
		response.KeysetIds = append(response.KeysetIds, &cwssaws.TenantKeysetIdentifier{KeysetId: id})
	}
	return &response, nil
}

// FindTenantKeysetsByIds implements interface NICoServer
func (f *NICoServerImpl) FindTenantKeysetsByIds(ctx context.Context, req *cwssaws.TenantKeysetsByIdsRequest) (*cwssaws.TenantKeySetList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.TenantKeySetList{}
	for _, id := range req.KeysetIds {
		if obj, ok := f.tk[id.KeysetId]; ok {
			response.Keyset = append(response.Keyset, obj)
		}
	}
	return &response, nil
}

// CreateIBPartition implements interface NICoServer
func (f *NICoServerImpl) CreateIBPartition(c context.Context, req *cwssaws.IBPartitionCreationRequest) (*cwssaws.IBPartition, error) {
	if req == nil || req.Config == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	nid := DefaultIBParitionId
	if req.Id != nil && req.Id.Value != "" {
		nid = req.Id.Value
	} else if _, ok := f.ibp[DefaultIBParitionId]; ok {
		// Default IBPartition already exists, create a new one with a different ID
		nid = uuid.NewString()
	}

	nibp := &cwssaws.IBPartition{
		Id: &cwssaws.IBPartitionId{Value: nid},
		Config: &cwssaws.IBPartitionConfig{
			Name:                 req.Config.Name,
			TenantOrganizationId: req.Config.TenantOrganizationId,
		},
		Status: &cwssaws.IBPartitionStatus{State: cwssaws.TenantState_READY},
	}

	f.ibp[nid] = nibp
	return nibp, nil
}

// UpdateIBPartition implements interface NICoServer
func (f *NICoServerImpl) UpdateIBPartition(c context.Context, req *cwssaws.IBPartitionUpdateRequest) (*cwssaws.IBPartition, error) {
	if req == nil || req.Id == nil || req.Id.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	ibp, ok := f.ibp[req.Id.Value]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "IB Partition with ID %q not found", req.Id.Value)
	}
	if req.Config != nil {
		if ibp.Config == nil {
			ibp.Config = &cwssaws.IBPartitionConfig{}
		}
		if req.Config.Name != "" {
			ibp.Config.Name = req.Config.Name
		}
		if req.Config.TenantOrganizationId != "" {
			ibp.Config.TenantOrganizationId = req.Config.TenantOrganizationId
		}
		if req.Config.Pkey != nil {
			ibp.Config.Pkey = req.Config.Pkey
		}
	}
	if req.Metadata != nil {
		ibp.Metadata = req.Metadata
	}
	return ibp, nil
}

// DeleteIBPartition implements interface NICoServer
func (f *NICoServerImpl) DeleteIBPartition(c context.Context, req *cwssaws.IBPartitionDeletionRequest) (*cwssaws.IBPartitionDeletionResult, error) {
	if req == nil || req.Id == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}

	_, ok := f.ibp[req.Id.Value]

	if ok {
		delete(f.ibp, req.Id.Value)
		return &cwssaws.IBPartitionDeletionResult{}, nil
	}

	return nil, status.Errorf(codes.NotFound, "IB Partition with ID %q not found", req.Id.Value)
}

// FindIBPartitionIds implements interface NICoServer
func (f *NICoServerImpl) FindIBPartitionIds(ctx context.Context, req *cwssaws.IBPartitionSearchFilter) (*cwssaws.IBPartitionIdList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.IBPartitionIdList{}
	for id := range f.ibp {
		response.IbPartitionIds = append(response.IbPartitionIds, &cwssaws.IBPartitionId{Value: id})
	}
	return &response, nil
}

// FindIBPartitionsByIds implements interface NICoServer
func (f *NICoServerImpl) FindIBPartitionsByIds(ctx context.Context, req *cwssaws.IBPartitionsByIdsRequest) (*cwssaws.IBPartitionList, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	response := cwssaws.IBPartitionList{}
	for _, id := range req.IbPartitionIds {
		if obj, ok := f.ibp[id.GetValue()]; ok {
			response.IbPartitions = append(response.IbPartitions, obj)
		}
	}
	return &response, nil
}

// AddExpectedMachine implements interface NICoServer
func (f *NICoServerImpl) AddExpectedMachine(ctx context.Context, req *cwssaws.ExpectedMachine) (*emptypb.Empty, error) {
	if req == nil || req.Id == nil || req.Id.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for AddExpectedMachine")
	}
	if req.BmcMacAddress == "" {
		return nil, status.Errorf(codes.InvalidArgument, "MAC address not provided for AddExpectedMachine")
	}
	if req.ChassisSerialNumber == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Chassis Serial Number not provided for AddExpectedMachine")
	}
	f.em[req.Id.Value] = req
	return &emptypb.Empty{}, nil
}

// UpdateExpectedMachine implements interface NICoServer
func (f *NICoServerImpl) UpdateExpectedMachine(ctx context.Context, req *cwssaws.ExpectedMachine) (*emptypb.Empty, error) {
	if req == nil || req.Id == nil || req.Id.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for UpdateExpectedMachine")
	}
	if req.BmcMacAddress == "" {
		return nil, status.Errorf(codes.InvalidArgument, "MAC address not provided for UpdateExpectedMachine")
	}
	if req.ChassisSerialNumber == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Chassis Serial Number not provided for UpdateExpectedMachine")
	}
	if _, ok := f.em[req.Id.Value]; !ok {
		return nil, status.Errorf(codes.NotFound, "ExpectedMachine with ID %q not found", req.Id.Value)
	}
	f.em[req.Id.Value] = req
	return &emptypb.Empty{}, nil
}

// DeleteExpectedMachine implements interface NICoServer
func (f *NICoServerImpl) DeleteExpectedMachine(ctx context.Context, req *cwssaws.ExpectedMachineRequest) (*emptypb.Empty, error) {
	if req == nil || req.Id == nil || req.Id.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for DeleteExpectedMachine")
	}
	if _, ok := f.em[req.Id.Value]; !ok {
		return nil, status.Errorf(codes.NotFound, "ExpectedMachine with ID %q not found", req.Id.Value)
	}
	delete(f.em, req.Id.Value)
	return &emptypb.Empty{}, nil
}

// CreateExpectedMachines implements interface NICoServer
func (f *NICoServerImpl) CreateExpectedMachines(ctx context.Context, req *cwssaws.BatchExpectedMachineOperationRequest) (*cwssaws.BatchExpectedMachineOperationResponse, error) {
	if req == nil || req.GetExpectedMachines() == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request for CreateExpectedMachines")
	}
	emList := req.GetExpectedMachines().GetExpectedMachines()
	out := &cwssaws.BatchExpectedMachineOperationResponse{
		Results: make([]*cwssaws.ExpectedMachineOperationResult, 0, len(emList)),
	}
	for _, em := range emList {
		if em == nil {
			msg := "nil expected machine entry"
			out.Results = append(out.Results, &cwssaws.ExpectedMachineOperationResult{
				Success:         false,
				ErrorMessage:    &msg,
				ExpectedMachine: nil,
			})
			continue
		}
		result := &cwssaws.ExpectedMachineOperationResult{
			Id:              em.Id,
			Success:         true,
			ExpectedMachine: em,
		}
		if em.GetId() == nil || em.GetId().GetValue() == "" {
			result.Success = false
			msg := "ID not provided"
			result.ErrorMessage = &msg
			result.ExpectedMachine = nil
		} else if em.GetBmcMacAddress() == "" {
			result.Success = false
			msg := "MAC address not provided"
			result.ErrorMessage = &msg
			result.ExpectedMachine = nil
		} else if em.GetChassisSerialNumber() == "" {
			result.Success = false
			msg := "Chassis Serial Number not provided"
			result.ErrorMessage = &msg
			result.ExpectedMachine = nil
		} else {
			f.em[em.Id.Value] = em
		}
		out.Results = append(out.Results, result)
	}
	return out, nil
}

// UpdateExpectedMachines implements interface NICoServer
func (f *NICoServerImpl) UpdateExpectedMachines(ctx context.Context, req *cwssaws.BatchExpectedMachineOperationRequest) (*cwssaws.BatchExpectedMachineOperationResponse, error) {
	if req == nil || req.GetExpectedMachines() == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request for UpdateExpectedMachines")
	}
	emList := req.GetExpectedMachines().GetExpectedMachines()
	out := &cwssaws.BatchExpectedMachineOperationResponse{
		Results: make([]*cwssaws.ExpectedMachineOperationResult, 0, len(emList)),
	}
	for _, em := range emList {
		if em == nil {
			msg := "nil expected machine entry"
			out.Results = append(out.Results, &cwssaws.ExpectedMachineOperationResult{
				Success:         false,
				ErrorMessage:    &msg,
				ExpectedMachine: nil,
			})
			continue
		}
		result := &cwssaws.ExpectedMachineOperationResult{
			Id:              em.Id,
			Success:         true,
			ExpectedMachine: em,
		}
		if em.GetId() == nil || em.GetId().GetValue() == "" {
			result.Success = false
			msg := "ID not provided"
			result.ErrorMessage = &msg
			result.ExpectedMachine = nil
		} else if em.GetBmcMacAddress() == "" {
			result.Success = false
			msg := "MAC address not provided"
			result.ErrorMessage = &msg
			result.ExpectedMachine = nil
		} else if em.GetChassisSerialNumber() == "" {
			result.Success = false
			msg := "Chassis Serial Number not provided"
			result.ErrorMessage = &msg
			result.ExpectedMachine = nil
		} else if _, ok := f.em[em.Id.Value]; !ok {
			result.Success = false
			msg := fmt.Sprintf("ExpectedMachine with ID %q not found", em.Id.Value)
			result.ErrorMessage = &msg
			result.ExpectedMachine = nil
		} else {
			f.em[em.Id.Value] = em
		}
		out.Results = append(out.Results, result)
	}
	return out, nil
}

// AddExpectedPowerShelf implements interface NICoServer
func (f *NICoServerImpl) AddExpectedPowerShelf(ctx context.Context, req *cwssaws.ExpectedPowerShelf) (*emptypb.Empty, error) {
	if req == nil || req.ExpectedPowerShelfId == nil || req.ExpectedPowerShelfId.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for AddExpectedPowerShelf")
	}
	if req.BmcMacAddress == "" {
		return nil, status.Errorf(codes.InvalidArgument, "MAC address not provided for AddExpectedPowerShelf")
	}
	if req.ShelfSerialNumber == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Shelf Serial Number not provided for AddExpectedPowerShelf")
	}
	f.eps[req.ExpectedPowerShelfId.Value] = req
	return &emptypb.Empty{}, nil
}

// UpdateExpectedPowerShelf implements interface NICoServer
func (f *NICoServerImpl) UpdateExpectedPowerShelf(ctx context.Context, req *cwssaws.ExpectedPowerShelf) (*emptypb.Empty, error) {
	if req == nil || req.ExpectedPowerShelfId == nil || req.ExpectedPowerShelfId.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for UpdateExpectedPowerShelf")
	}
	if req.BmcMacAddress == "" {
		return nil, status.Errorf(codes.InvalidArgument, "MAC address not provided for UpdateExpectedPowerShelf")
	}
	if req.ShelfSerialNumber == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Shelf Serial Number not provided for UpdateExpectedPowerShelf")
	}
	if _, ok := f.eps[req.ExpectedPowerShelfId.Value]; !ok {
		return nil, status.Errorf(codes.NotFound, "ExpectedPowerShelf with ID %q not found", req.ExpectedPowerShelfId.Value)
	}
	f.eps[req.ExpectedPowerShelfId.Value] = req
	return &emptypb.Empty{}, nil
}

// DeleteExpectedPowerShelf implements interface NICoServer
func (f *NICoServerImpl) DeleteExpectedPowerShelf(ctx context.Context, req *cwssaws.ExpectedPowerShelfRequest) (*emptypb.Empty, error) {
	if req == nil || req.ExpectedPowerShelfId == nil || req.ExpectedPowerShelfId.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for DeleteExpectedPowerShelf")
	}
	if _, ok := f.eps[req.ExpectedPowerShelfId.Value]; !ok {
		return nil, status.Errorf(codes.NotFound, "ExpectedPowerShelf with ID %q not found", req.ExpectedPowerShelfId.Value)
	}
	delete(f.eps, req.ExpectedPowerShelfId.Value)
	return &emptypb.Empty{}, nil
}

// GetExpectedPowerShelf implements interface NICoServer
func (f *NICoServerImpl) GetExpectedPowerShelf(ctx context.Context, req *cwssaws.ExpectedPowerShelfRequest) (*cwssaws.ExpectedPowerShelf, error) {
	if req == nil || req.ExpectedPowerShelfId == nil || req.ExpectedPowerShelfId.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for GetExpectedPowerShelf")
	}
	eps, ok := f.eps[req.ExpectedPowerShelfId.Value]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "ExpectedPowerShelf with ID %q not found", req.ExpectedPowerShelfId.Value)
	}
	return eps, nil
}

// GetAllExpectedPowerShelves implements interface NICoServer
func (f *NICoServerImpl) GetAllExpectedPowerShelves(ctx context.Context, req *emptypb.Empty) (*cwssaws.ExpectedPowerShelfList, error) {
	res := make([]*cwssaws.ExpectedPowerShelf, 0, len(f.eps))
	for _, eps := range f.eps {
		res = append(res, eps)
	}
	return &cwssaws.ExpectedPowerShelfList{ExpectedPowerShelves: res}, nil
}

// AddExpectedSwitch implements interface NICoServer
func (f *NICoServerImpl) AddExpectedSwitch(ctx context.Context, req *cwssaws.ExpectedSwitch) (*emptypb.Empty, error) {
	if req == nil || req.ExpectedSwitchId == nil || req.ExpectedSwitchId.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for AddExpectedSwitch")
	}
	if req.BmcMacAddress == "" {
		return nil, status.Errorf(codes.InvalidArgument, "MAC address not provided for AddExpectedSwitch")
	}
	if req.SwitchSerialNumber == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Switch Serial Number not provided for AddExpectedSwitch")
	}
	f.es[req.ExpectedSwitchId.Value] = req
	return &emptypb.Empty{}, nil
}

// UpdateExpectedSwitch implements interface NICoServer
func (f *NICoServerImpl) UpdateExpectedSwitch(ctx context.Context, req *cwssaws.ExpectedSwitch) (*emptypb.Empty, error) {
	if req == nil || req.ExpectedSwitchId == nil || req.ExpectedSwitchId.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for UpdateExpectedSwitch")
	}
	if req.BmcMacAddress == "" {
		return nil, status.Errorf(codes.InvalidArgument, "MAC address not provided for UpdateExpectedSwitch")
	}
	if req.SwitchSerialNumber == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Switch Serial Number not provided for UpdateExpectedSwitch")
	}
	if _, ok := f.es[req.ExpectedSwitchId.Value]; !ok {
		return nil, status.Errorf(codes.NotFound, "ExpectedSwitch with ID %q not found", req.ExpectedSwitchId.Value)
	}
	f.es[req.ExpectedSwitchId.Value] = req
	return &emptypb.Empty{}, nil
}

// DeleteExpectedSwitch implements interface NICoServer
func (f *NICoServerImpl) DeleteExpectedSwitch(ctx context.Context, req *cwssaws.ExpectedSwitchRequest) (*emptypb.Empty, error) {
	if req == nil || req.ExpectedSwitchId == nil || req.ExpectedSwitchId.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for DeleteExpectedSwitch")
	}
	if _, ok := f.es[req.ExpectedSwitchId.Value]; !ok {
		return nil, status.Errorf(codes.NotFound, "ExpectedSwitch with ID %q not found", req.ExpectedSwitchId.Value)
	}
	delete(f.es, req.ExpectedSwitchId.Value)
	return &emptypb.Empty{}, nil
}

// GetExpectedSwitch implements interface NICoServer
func (f *NICoServerImpl) GetExpectedSwitch(ctx context.Context, req *cwssaws.ExpectedSwitchRequest) (*cwssaws.ExpectedSwitch, error) {
	if req == nil || req.ExpectedSwitchId == nil || req.ExpectedSwitchId.Value == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for GetExpectedSwitch")
	}
	es, ok := f.es[req.ExpectedSwitchId.Value]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "ExpectedSwitch with ID %q not found", req.ExpectedSwitchId.Value)
	}
	return es, nil
}

// GetAllExpectedSwitches implements interface NICoServer
func (f *NICoServerImpl) GetAllExpectedSwitches(ctx context.Context, req *emptypb.Empty) (*cwssaws.ExpectedSwitchList, error) {
	res := make([]*cwssaws.ExpectedSwitch, 0, len(f.es))
	for _, es := range f.es {
		res = append(res, es)
	}
	return &cwssaws.ExpectedSwitchList{ExpectedSwitches: res}, nil
}

// AddExpectedRack implements interface NICoServer
func (f *NICoServerImpl) AddExpectedRack(ctx context.Context, req *cwssaws.ExpectedRack) (*emptypb.Empty, error) {
	if req == nil || req.RackId == nil || req.RackId.Id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for AddExpectedRack")
	}
	if req.RackProfileId == nil || req.RackProfileId.Id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Rack Profile ID not provided for AddExpectedRack")
	}
	f.er[req.RackId.Id] = req
	return &emptypb.Empty{}, nil
}

// UpdateExpectedRack implements interface NICoServer
func (f *NICoServerImpl) UpdateExpectedRack(ctx context.Context, req *cwssaws.ExpectedRack) (*emptypb.Empty, error) {
	if req == nil || req.RackId == nil || req.RackId.Id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for UpdateExpectedRack")
	}
	if req.RackProfileId == nil || req.RackProfileId.Id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Rack Profile ID not provided for UpdateExpectedRack")
	}
	if _, ok := f.er[req.RackId.Id]; !ok {
		return nil, status.Errorf(codes.NotFound, "ExpectedRack with ID %q not found", req.RackId.Id)
	}
	f.er[req.RackId.Id] = req
	return &emptypb.Empty{}, nil
}

// DeleteExpectedRack implements interface NICoServer
func (f *NICoServerImpl) DeleteExpectedRack(ctx context.Context, req *cwssaws.ExpectedRackRequest) (*emptypb.Empty, error) {
	if req == nil || req.RackId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for DeleteExpectedRack")
	}
	if _, ok := f.er[req.RackId]; !ok {
		return nil, status.Errorf(codes.NotFound, "ExpectedRack with ID %q not found", req.RackId)
	}
	delete(f.er, req.RackId)
	return &emptypb.Empty{}, nil
}

// GetExpectedRack implements interface NICoServer
func (f *NICoServerImpl) GetExpectedRack(ctx context.Context, req *cwssaws.ExpectedRackRequest) (*cwssaws.ExpectedRack, error) {
	if req == nil || req.RackId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "ID not provided for GetExpectedRack")
	}
	er, ok := f.er[req.RackId]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "ExpectedRack with ID %q not found", req.RackId)
	}
	return er, nil
}

// GetAllExpectedRacks implements interface NICoServer
func (f *NICoServerImpl) GetAllExpectedRacks(ctx context.Context, req *emptypb.Empty) (*cwssaws.ExpectedRackList, error) {
	res := make([]*cwssaws.ExpectedRack, 0, len(f.er))
	for _, er := range f.er {
		res = append(res, er)
	}
	return &cwssaws.ExpectedRackList{ExpectedRacks: res}, nil
}

// ReplaceAllExpectedRacks implements interface NICoServer
func (f *NICoServerImpl) ReplaceAllExpectedRacks(ctx context.Context, req *cwssaws.ExpectedRackList) (*emptypb.Empty, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	for _, er := range req.ExpectedRacks {
		if er == nil || er.RackId == nil || er.RackId.Id == "" {
			return nil, status.Errorf(codes.InvalidArgument, "ID not provided for ReplaceAllExpectedRacks")
		}
		if er.RackProfileId == nil || er.RackProfileId.Id == "" {
			return nil, status.Errorf(codes.InvalidArgument, "Rack Profile ID not provided for ReplaceAllExpectedRacks")
		}
	}
	f.er = make(map[string]*cwssaws.ExpectedRack)
	for _, er := range req.ExpectedRacks {
		f.er[er.RackId.Id] = er
	}
	return &emptypb.Empty{}, nil
}

// DeleteAllExpectedRacks implements interface NICoServer
func (f *NICoServerImpl) DeleteAllExpectedRacks(ctx context.Context, req *emptypb.Empty) (*emptypb.Empty, error) {
	f.er = make(map[string]*cwssaws.ExpectedRack)
	return &emptypb.Empty{}, nil
}

// loadFromInventory seeds machines and expected machines from YAML config.
func (f *NICoServerImpl) loadFromInventory(inv *config.Inventory) {
	if inv == nil {
		return
	}
	for id, machine := range inv.Machines {
		f.m[id] = machine
		mockdata.EnsureMachineDiscoveryInfo(f.m[id])
	}
	for _, em := range inv.ExpectedMachines {
		if em.GetId() != nil && em.GetId().GetValue() != "" {
			f.em[em.GetId().GetValue()] = em
		}
	}
	f.linkedExpectedMachines = append([]*cwssaws.LinkedExpectedMachine(nil), inv.LinkedExpectedMachines...)
}

// ~~~~~ Machine Identity mock methods ~~~~~ //

const (
	jwtESAlg          = "ES256"
	p256CoordinateLen = 32
)

// generateES256KeyMaterial returns a fresh P-256 keypair with derived kid.
func generateES256KeyMaterial() (*identityKeyMaterial, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate P-256 key: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal SPKI: %w", err)
	}
	publicPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki}))
	sum := sha256.Sum256([]byte(publicPEM))
	return &identityKeyMaterial{
		privateKey: priv,
		publicPEM:  publicPEM,
		kid:        hex.EncodeToString(sum[:]),
	}, nil
}

// jwksDocumentForKey returns a one-key JWKS JSON document.
func jwksDocumentForKey(km *identityKeyMaterial, use string) (string, error) {
	return jwksDocumentForKeys([]*identityKeyMaterial{km}, use)
}

// jwksDocumentForKeys returns a JWKS JSON document for the supplied keys
// (two during a rotation overlap window; one in steady state).
func jwksDocumentForKeys(kms []*identityKeyMaterial, use string) (string, error) {
	jwks := make([]map[string]string, 0, len(kms))
	for _, km := range kms {
		if km == nil || km.privateKey == nil {
			return "", fmt.Errorf("nil key material")
		}
		pub := km.privateKey.PublicKey
		xb := pub.X.FillBytes(make([]byte, p256CoordinateLen))
		yb := pub.Y.FillBytes(make([]byte, p256CoordinateLen))
		jwks = append(jwks, map[string]string{
			"kty": "EC",
			"crv": "P-256",
			"alg": jwtESAlg,
			"use": use,
			"kid": km.kid,
			"x":   base64.RawURLEncoding.EncodeToString(xb),
			"y":   base64.RawURLEncoding.EncodeToString(yb),
		})
	}
	out, err := json.Marshal(map[string]any{"keys": jwks})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// signES256JWT returns a compact-serialized ES256 JWS.
func signES256JWT(priv *ecdsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header := map[string]string{"alg": jwtESAlg, "kid": kid, "typ": "JWT"}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal JWT header: %w", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal JWT claims: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." +
		base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return "", fmt.Errorf("ecdsa sign: %w", err)
	}
	sig := make([]byte, 2*p256CoordinateLen)
	r.FillBytes(sig[:p256CoordinateLen])
	s.FillBytes(sig[p256CoordinateLen:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// clientSecretDisplayHash returns the truncated SHA-256 display form.
func clientSecretDisplayHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	full := hex.EncodeToString(sum[:])
	if len(full) >= 8 {
		return "sha256:" + full[:8] + ".."
	}
	return "sha256:" + full
}

// resolveSubjectPrefix mirrors carbide-core's `resolve_subject_prefix`:
// derives `spiffe://<trust-domain-from-issuer>` from the issuer URL host.
func resolveSubjectPrefix(issuer string) string {
	issuer = strings.TrimSpace(issuer)
	if issuer == "" {
		return ""
	}
	u, err := url.Parse(issuer)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ""
	}
	return "spiffe://" + host
}

// normalizeAllowedAudiences defaults to [defaultAud] when empty; otherwise defaultAud must appear in allowed.
func normalizeAllowedAudiences(defaultAud string, allowed []string) ([]string, error) {
	if len(allowed) == 0 {
		return []string{defaultAud}, nil
	}
	for _, a := range allowed {
		if a == defaultAud {
			out := make([]string, len(allowed))
			copy(out, allowed)
			return out, nil
		}
	}
	return nil, fmt.Errorf("default_audience %q must appear in allowed_audiences", defaultAud)
}

// SetTenantIdentityConfiguration implements interface NICoServer
func (f *NICoServerImpl) SetTenantIdentityConfiguration(ctx context.Context, req *cwssaws.SetTenantIdentityConfigRequest) (*cwssaws.TenantIdentityConfigResponse, error) {
	if req == nil || req.GetOrganizationId() == "" || req.GetConfig() == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	in := req.GetConfig()
	if strings.TrimSpace(in.GetIssuer()) == "" {
		return nil, status.Errorf(codes.InvalidArgument, "issuer is required")
	}
	if strings.TrimSpace(in.GetDefaultAudience()) == "" {
		return nil, status.Errorf(codes.InvalidArgument, "default_audience is required")
	}
	if in.GetTokenTtlSec() == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "token_ttl_sec must be greater than zero")
	}
	allowed, err := normalizeAllowedAudiences(in.GetDefaultAudience(), in.GetAllowedAudiences())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
	}

	switch {
	case in.GetRotateKey() && in.SigningKeyOverlapSec == nil:
		return nil, status.Errorf(codes.InvalidArgument, "signing_key_overlap_sec is required when rotate_key is true")
	case !in.GetRotateKey() && in.SigningKeyOverlapSec != nil:
		return nil, status.Errorf(codes.InvalidArgument, "signing_key_overlap_sec must be omitted when rotate_key is false")
	case in.GetRotateKey() && in.SigningKeyOverlapSec != nil && *in.SigningKeyOverlapSec < in.GetTokenTtlSec():
		return nil, status.Errorf(codes.InvalidArgument, "signing_key_overlap_sec must be >= token_ttl_sec")
	}

	orgID := req.GetOrganizationId()
	f.gcExpiredNonActiveSigningKey(orgID)
	now := timestamppb.Now()
	st, isUpdate := f.identityState[orgID]
	if !isUpdate {
		st = &identityOrgState{}
		f.identityState[orgID] = st
	}

	switch {
	case !isUpdate:
		newKey, err := generateES256KeyMaterial()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to generate signing key: %v", err)
		}
		st.slot1 = newKey
		st.slot2 = nil
		st.currentSlot = 1
		st.nonActiveSlotExpires = nil
	case in.GetRotateKey():
		st.clearInactiveSlot()
		newKey, err := generateES256KeyMaterial()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to generate signing key: %v", err)
		}
		st.setSlot(st.inactiveSlot(), newKey)
		st.currentSlot = st.inactiveSlot()
		expire := now.AsTime().Add(time.Duration(*in.SigningKeyOverlapSec) * time.Second)
		st.nonActiveSlotExpires = &expire
	}

	resolvedSubjectPrefix := in.SubjectPrefix
	if resolvedSubjectPrefix == nil || strings.TrimSpace(*resolvedSubjectPrefix) == "" {
		if derived := resolveSubjectPrefix(in.GetIssuer()); derived != "" {
			resolvedSubjectPrefix = &derived
		}
	}

	resp := &cwssaws.TenantIdentityConfigResponse{
		OrganizationId: orgID,
		Config: &cwssaws.TenantIdentityConfig{
			Enabled:          in.GetEnabled(),
			Issuer:           in.GetIssuer(),
			DefaultAudience:  in.GetDefaultAudience(),
			AllowedAudiences: allowed,
			TokenTtlSec:      in.GetTokenTtlSec(),
			SubjectPrefix:    resolvedSubjectPrefix,
			RotateKey:        st.nonActiveSlotExpires != nil,
		},
		UpdatedAt: now,
	}
	if isUpdate {
		resp.CreatedAt = st.cfg.GetCreatedAt()
	} else {
		resp.CreatedAt = now
	}
	resp.SigningKeys = tenantIdentitySigningKeysResponse(st)
	st.cfg = resp
	return resp, nil
}

// GetTenantIdentityConfiguration implements interface NICoServer
func (f *NICoServerImpl) GetTenantIdentityConfiguration(ctx context.Context, req *cwssaws.GetTenantIdentityConfigRequest) (*cwssaws.TenantIdentityConfigResponse, error) {
	if req == nil || req.GetOrganizationId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	orgID := req.GetOrganizationId()
	f.gcExpiredNonActiveSigningKey(orgID)
	st, ok := f.identityState[orgID]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Identity configuration not found for org %q", orgID)
	}
	resp := cloneTenantIdentityConfigResponse(st.cfg)
	resp.SigningKeys = tenantIdentitySigningKeysResponse(st)
	if cfg := resp.GetConfig(); cfg != nil {
		cfg.RotateKey = st.nonActiveSlotExpires != nil
	}
	return resp, nil
}

func cloneTenantIdentityConfigResponse(in *cwssaws.TenantIdentityConfigResponse) *cwssaws.TenantIdentityConfigResponse {
	if in == nil {
		return nil
	}
	out := &cwssaws.TenantIdentityConfigResponse{
		OrganizationId: in.GetOrganizationId(),
		CreatedAt:      in.GetCreatedAt(),
		UpdatedAt:      in.GetUpdatedAt(),
	}
	if cfg := in.GetConfig(); cfg != nil {
		out.Config = &cwssaws.TenantIdentityConfig{
			Enabled:              cfg.GetEnabled(),
			Issuer:               cfg.GetIssuer(),
			DefaultAudience:      cfg.GetDefaultAudience(),
			AllowedAudiences:     cfg.GetAllowedAudiences(),
			TokenTtlSec:          cfg.GetTokenTtlSec(),
			SubjectPrefix:        cfg.SubjectPrefix,
			RotateKey:            cfg.GetRotateKey(),
			SigningKeyOverlapSec: cfg.SigningKeyOverlapSec,
		}
	}
	return out
}

// DeleteTenantIdentityConfiguration implements interface NICoServer
func (f *NICoServerImpl) DeleteTenantIdentityConfiguration(ctx context.Context, req *cwssaws.GetTenantIdentityConfigRequest) (*emptypb.Empty, error) {
	if req == nil || req.GetOrganizationId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	orgID := req.GetOrganizationId()
	if _, ok := f.identityState[orgID]; !ok {
		return nil, status.Errorf(codes.NotFound, "Identity configuration not found for org %q", orgID)
	}
	delete(f.identityState, orgID)
	delete(f.tokenDelegations, orgID)
	return &emptypb.Empty{}, nil
}

// SetTokenDelegation implements interface NICoServer
func (f *NICoServerImpl) SetTokenDelegation(ctx context.Context, req *cwssaws.TokenDelegationRequest) (*cwssaws.TokenDelegationResponse, error) {
	if req == nil || req.GetOrganizationId() == "" || req.GetConfig() == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	orgID := req.GetOrganizationId()
	in := req.GetConfig()
	if strings.TrimSpace(in.GetTokenEndpoint()) == "" {
		return nil, status.Errorf(codes.InvalidArgument, "token_endpoint is required")
	}
	if strings.TrimSpace(in.GetSubjectTokenAudience()) == "" {
		return nil, status.Errorf(codes.InvalidArgument, "subject_token_audience is required")
	}

	if _, ok := f.identityState[orgID]; !ok {
		return nil, status.Errorf(codes.NotFound, "Identity configuration must exist before token delegation is set for org %q", orgID)
	}

	now := timestamppb.Now()
	existing, isUpdate := f.tokenDelegations[orgID]

	resp := &cwssaws.TokenDelegationResponse{
		OrganizationId:       orgID,
		TokenEndpoint:        in.GetTokenEndpoint(),
		SubjectTokenAudience: in.GetSubjectTokenAudience(),
		UpdatedAt:            now,
	}
	if basic := in.GetClientSecretBasic(); basic != nil {
		if strings.TrimSpace(basic.GetClientId()) == "" {
			return nil, status.Errorf(codes.InvalidArgument, "client_id is required for client_secret_basic")
		}
		if strings.TrimSpace(basic.GetClientSecret()) == "" {
			return nil, status.Errorf(codes.InvalidArgument, "client_secret is required for client_secret_basic")
		}
		resp.AuthMethodConfig = &cwssaws.TokenDelegationResponse_ClientSecretBasic{
			ClientSecretBasic: &cwssaws.ClientSecretBasicResponse{
				ClientId:         basic.GetClientId(),
				ClientSecretHash: clientSecretDisplayHash(basic.GetClientSecret()),
			},
		}
	}
	if isUpdate {
		resp.CreatedAt = existing.GetCreatedAt()
	} else {
		resp.CreatedAt = now
	}

	f.tokenDelegations[orgID] = resp
	return resp, nil
}

// GetTokenDelegation implements interface NICoServer
func (f *NICoServerImpl) GetTokenDelegation(ctx context.Context, req *cwssaws.GetTokenDelegationRequest) (*cwssaws.TokenDelegationResponse, error) {
	if req == nil || req.GetOrganizationId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	td, ok := f.tokenDelegations[req.GetOrganizationId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Token delegation not found for org %q", req.GetOrganizationId())
	}
	return td, nil
}

// DeleteTokenDelegation implements interface NICoServer. Idempotent: a
// missing entry returns success (matches carbide-core's no-op delete).
func (f *NICoServerImpl) DeleteTokenDelegation(ctx context.Context, req *cwssaws.GetTokenDelegationRequest) (*emptypb.Empty, error) {
	if req == nil || req.GetOrganizationId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	delete(f.tokenDelegations, req.GetOrganizationId())
	return &emptypb.Empty{}, nil
}

// GetJWKS implements interface NICoServer
func (f *NICoServerImpl) GetJWKS(ctx context.Context, req *cwssaws.JwksRequest) (*cwssaws.Jwks, error) {
	if req == nil || req.GetOrganizationId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	orgID := req.GetOrganizationId()
	f.gcExpiredNonActiveSigningKey(orgID)
	st, hasCfg := f.identityState[orgID]
	if !hasCfg {
		return nil, status.Errorf(codes.NotFound, "Identity configuration not found for org %q", orgID)
	}
	use := "sig"
	if req.GetKind() == cwssaws.JwksKind_Spiffe {
		use = "jwt-svid"
	}
	keys := []*identityKeyMaterial{}
	if active := st.activeKey(); active != nil {
		keys = append(keys, active)
	}
	if inactive := st.inactiveKey(); inactive != nil {
		keys = append(keys, inactive)
	}
	if len(keys) == 0 {
		return nil, status.Errorf(codes.Internal, "Signing key missing for org %q (mock state inconsistent)", orgID)
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].kid < keys[j].kid })
	doc, err := jwksDocumentForKeys(keys, use)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to serialize JWKS: %v", err)
	}
	return &cwssaws.Jwks{Jwks: doc}, nil
}

// GetOpenIDConfiguration implements interface NICoServer
func (f *NICoServerImpl) GetOpenIDConfiguration(ctx context.Context, req *cwssaws.OpenIdConfigRequest) (*cwssaws.OpenIdConfiguration, error) {
	if req == nil || req.GetOrganizationId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	orgID := req.GetOrganizationId()
	f.gcExpiredNonActiveSigningKey(orgID)
	st, ok := f.identityState[orgID]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Identity configuration not found for org %q", orgID)
	}
	iss := st.cfg.GetConfig().GetIssuer()
	if strings.TrimSpace(iss) == "" {
		return nil, status.Errorf(codes.NotFound, "Issuer not configured for org %q", orgID)
	}
	if st.activeKey() == nil {
		return nil, status.Errorf(codes.NotFound, "No active signing key for org %q", orgID)
	}
	base := strings.TrimRight(iss, "/")
	return &cwssaws.OpenIdConfiguration{
		Issuer:                           iss,
		JwksUri:                          base + "/.well-known/jwks.json",
		ResponseTypesSupported:           []string{"token"},
		SubjectTypesSupported:            []string{"public"},
		IdTokenSigningAlgValuesSupported: []string{},
		SpiffeJwksUri:                    base + "/.well-known/spiffe/jwks.json",
	}, nil
}

// resolveSigningOrg returns the seeded org when exactly one identity is configured,
// otherwise the empty string.
func (f *NICoServerImpl) resolveSigningOrg(_ context.Context) string {
	if len(f.identityState) == 1 {
		for k := range f.identityState {
			return k
		}
	}
	return ""
}

// SignMachineIdentity implements interface NICoServer
func (f *NICoServerImpl) SignMachineIdentity(ctx context.Context, req *cwssaws.MachineIdentityRequest) (*cwssaws.MachineIdentityResponse, error) {
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request argument")
	}
	orgID := f.resolveSigningOrg(ctx)
	if orgID == "" {
		return nil, status.Errorf(codes.Unauthenticated, "Cannot resolve signing org; seed exactly one identity in the mock")
	}
	st, hasCfg := f.identityState[orgID]
	if !hasCfg {
		return nil, status.Errorf(codes.NotFound, "Identity configuration not found for org %q", orgID)
	}
	km := st.activeKey()
	if km == nil {
		return nil, status.Errorf(codes.Internal, "Signing key missing for org %q (mock state inconsistent)", orgID)
	}
	cfg := st.cfg

	audiences := req.GetAudience()
	if len(audiences) == 0 {
		audiences = []string{cfg.GetConfig().GetDefaultAudience()}
	} else {
		allowed := make(map[string]struct{}, len(cfg.GetConfig().GetAllowedAudiences()))
		for _, a := range cfg.GetConfig().GetAllowedAudiences() {
			allowed[a] = struct{}{}
		}
		for _, a := range audiences {
			if _, ok := allowed[a]; !ok {
				return nil, status.Errorf(codes.InvalidArgument, "audience %q is not in allowed_audiences", a)
			}
		}
	}

	ttl := int64(cfg.GetConfig().GetTokenTtlSec())
	if ttl <= 0 {
		ttl = 600
	}
	now := time.Now().Unix()

	subjectPrefix := cfg.GetConfig().GetSubjectPrefix()
	if subjectPrefix == "" {
		subjectPrefix = strings.TrimRight(cfg.GetConfig().GetIssuer(), "/") + "/machine"
	}
	sub := strings.TrimRight(subjectPrefix, "/") + "/" + uuid.NewString()

	var aud any = audiences[0]
	if len(audiences) > 1 {
		aud = audiences
	}
	claims := map[string]any{
		"iss": cfg.GetConfig().GetIssuer(),
		"sub": sub,
		"aud": aud,
		"iat": now,
		"nbf": now,
		"exp": now + ttl,
		"jti": uuid.NewString(),
	}

	token, err := signES256JWT(km.privateKey, km.kid, claims)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to sign token: %v", err)
	}
	return &cwssaws.MachineIdentityResponse{
		AccessToken:     token,
		IssuedTokenType: "urn:ietf:params:oauth:token-type:jwt",
		TokenType:       "Bearer",
		ExpiresInSec:    uint32(ttl),
	}, nil
}

// LoadTestIdentity seeds one example identity configuration.
func (f *NICoServerImpl) LoadTestIdentity() {
	const seedOrg = "test-org"
	if f.identityState == nil {
		f.identityState = make(map[string]*identityOrgState)
	}
	km, err := generateES256KeyMaterial()
	if err != nil {
		logger.Fatal().Err(err).Msg("LoadTestIdentity: failed to generate ES256 keypair")
		return
	}
	now := timestamppb.Now()
	st := &identityOrgState{
		slot1:       km,
		currentSlot: 1,
	}
	st.cfg = &cwssaws.TenantIdentityConfigResponse{
		OrganizationId: seedOrg,
		Config: &cwssaws.TenantIdentityConfig{
			Enabled:          true,
			Issuer:           "https://carbide-rest.mock/v2/org/test-org/nico/site/mock-site",
			DefaultAudience:  "openbao",
			AllowedAudiences: []string{"openbao", "vault"},
			TokenTtlSec:      600,
		},
		CreatedAt:   now,
		UpdatedAt:   now,
		SigningKeys: tenantIdentitySigningKeysResponse(st),
	}
	f.identityState[seedOrg] = st
}
