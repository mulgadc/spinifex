package vm_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// metadataFields are carried by resource.Metadata rather than by either half.
var metadataFields = map[string]string{
	"ID":                "Metadata.Name",
	"AccountID":         "Metadata.AccountID",
	"DeletionTimestamp": "Metadata.DeletionTimestamp",
}

// renamedFields are carried under a different name because the record drops the
// mutex each wrapper holds: a lock is process state, not a described fact.
var renamedFields = map[string]string{
	"EBSRequests": "Status.EBSRequests, minus its mutex",
	"ENIRequests": "Status.ENIAvailableSlots + Status.ENIAttachedByID, minus its mutex",
}

// notCarried are the fields that deliberately do not reach the record, each
// with the reason it does not.
var notCarried = map[string]string{
	"QMPClient":          "a live connection, already json:\"-\"",
	"LegacyAttributes":   "decode-only, folds records at the keys this one replaces",
	"LegacyExtraHostfwd": "decode-only, same",
}

// Every exported field of VM must be assigned somewhere. A field added to VM
// and to neither half would otherwise be dropped silently on the way to the
// record, which is the failure the whole split is meant to make impossible.
func TestRecord_CoversEveryVMField(t *testing.T) {
	specFields := exportedFields(reflect.TypeFor[vm.InstanceSpec]())
	statusFields := exportedFields(reflect.TypeFor[vm.InstanceStatus]())

	for _, name := range exportedFields(reflect.TypeFor[vm.VM]()) {
		t.Run(name, func(t *testing.T) {
			switch {
			case slices.Contains(specFields, name):
			case slices.Contains(statusFields, name):
			case metadataFields[name] != "":
			case renamedFields[name] != "":
			case notCarried[name] != "":
			default:
				t.Fatalf("VM.%s reaches neither InstanceSpec nor InstanceStatus.\n"+
					"Add it to the half that owns it — spec if the API layer writes it, status if the "+
					"node does — or to notCarried in this file with the reason it does not persist.", name)
			}
		})
	}
}

// The converse: a field on either half that VM does not have is one nothing can
// populate, so the record would carry a value no reader could trust.
func TestRecord_HalvesHaveNoFieldsVMLacks(t *testing.T) {
	vmFields := exportedFields(reflect.TypeFor[vm.VM]())
	renamed := []string{
		"EBSRequests", "ENIAvailableSlots", "ENIAttachedByID",
	}

	for _, half := range []reflect.Type{reflect.TypeFor[vm.InstanceSpec](), reflect.TypeFor[vm.InstanceStatus]()} {
		for _, name := range exportedFields(half) {
			if slices.Contains(renamed, name) {
				continue
			}
			assert.Contains(t, vmFields, name,
				"%s.%s has no source on VM, so nothing can populate it", half.Name(), name)
		}
	}
}

// No field on either half may hold a lock: the record is serialised, copied and
// compared, and a mutex that survives into it is a lock nobody owns.
func TestRecord_HalvesHoldNoLocks(t *testing.T) {
	for _, half := range []reflect.Type{reflect.TypeFor[vm.InstanceSpec](), reflect.TypeFor[vm.InstanceStatus]()} {
		for f := range half.Fields() {
			assert.False(t, containsLock(f.Type, 0),
				"%s.%s contains a sync lock; carry the payload without its wrapper", half.Name(), f.Name)
		}
	}
}

func TestRecord_RoundTrip(t *testing.T) {
	original := populatedVM()

	rebuilt := vm.VMFromRecord(original.Record())

	assertVMFieldsEqual(t, original, rebuilt)
}

// The record is persisted through kvstore, so the round trip has to survive
// JSON, not just the two conversions.
func TestRecord_RoundTripThroughJSON(t *testing.T) {
	original := populatedVM()

	encoded, err := json.Marshal(original.Record())
	require.NoError(t, err)

	var decoded vm.InstanceRecord
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	assertVMFieldsEqual(t, original, vm.VMFromRecord(&decoded))
}

// The mutexes are dropped on the way out, so the rebuilt instance must arrive
// with usable zero-value locks rather than ones somebody else left held.
func TestVMFromRecord_RequestWrappersAreUsable(t *testing.T) {
	rebuilt := vm.VMFromRecord(populatedVM().Record())

	assert.True(t, rebuilt.EBSRequests.Mu.TryLock(), "EBS request lock must arrive unheld")
	assert.True(t, rebuilt.ENIRequests.Mu.TryLock(), "ENI request lock must arrive unheld")

	assert.Nil(t, rebuilt.QMPClient, "the caller attaches a live connection; the record cannot carry one")
}

// An empty record must not decode into an instance carrying maps and slices
// that were never written.
func TestRecord_EmptyRoundTrips(t *testing.T) {
	rebuilt := vm.VMFromRecord((&vm.VM{}).Record())

	assert.Empty(t, rebuilt.ID)
	assert.Nil(t, rebuilt.EBSRequests.Requests)
	assert.Nil(t, rebuilt.ENIRequests.AttachedByENIID)
	assert.Nil(t, rebuilt.HostfwdPortMap)
	assert.Nil(t, rebuilt.DeletionTimestamp)
}

// The round trip can only catch a dropped field if the fixture sets one, so a
// field added to VM and to the record but not to populatedVM would compare
// zero against zero and pass. Guarding the fixture is what stops that.
func TestPopulatedVM_LeavesNoCarriedFieldZero(t *testing.T) {
	v := reflect.ValueOf(populatedVM()).Elem()
	typ := reflect.TypeFor[vm.VM]()

	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() || notCarried[f.Name] != "" {
			continue
		}
		assert.False(t, v.Field(i).IsZero(),
			"populatedVM leaves VM.%s at its zero value, so the round-trip tests would pass without carrying it", f.Name)
	}
}

// assertVMFieldsEqual compares every exported field except the ones the record
// deliberately drops, so a dropped field fails as itself rather than as a
// whole-struct diff.
func assertVMFieldsEqual(t *testing.T, want, got *vm.VM) {
	t.Helper()
	// Elem on the pointer rather than dereferencing: VM carries a mutex, so a
	// copy of it is one govet rejects and nothing here needs.
	wantV, gotV := reflect.ValueOf(want).Elem(), reflect.ValueOf(got).Elem()
	typ := reflect.TypeFor[vm.VM]()

	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() || notCarried[f.Name] != "" {
			continue
		}
		// The wrappers hold a mutex, so compare their payloads instead.
		switch f.Name {
		case "EBSRequests":
			assert.Equal(t, want.EBSRequests.Requests, got.EBSRequests.Requests)
			continue
		case "ENIRequests":
			assert.Equal(t, want.ENIRequests.AvailableSlots, got.ENIRequests.AvailableSlots)
			assert.Equal(t, want.ENIRequests.AttachedByENIID, got.ENIRequests.AttachedByENIID)
			continue
		}
		assert.Equal(t, wantV.Field(i).Interface(), gotV.Field(i).Interface(),
			"VM.%s did not survive the round trip", f.Name)
	}
}

// populatedVM sets every carried field to a distinctive value, so a field the
// conversion forgets shows up as a zero rather than matching by luck.
func populatedVM() *vm.VM {
	deleted := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	v := &vm.VM{
		ID:                "i-round-trip",
		AccountID:         "111122223333",
		DeletionTimestamp: &deleted,

		InstanceType:          "m7i.large",
		Config:                vm.Config{InstanceType: "m7i.large", MachineType: "q35"},
		RunInstancesInput:     &ec2.RunInstancesInput{ImageId: aws.String("ami-1")},
		DesiredState:          vm.DesiredStopped,
		HostfwdPorts:          []int{8080, 8443},
		ManagedBy:             "elbv2",
		BootMode:              "uefi",
		DirectBoot:            true,
		IamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/p",
		PlacementGroupName:    "pg-1",
		CapacityReservationId: "cr-1",
		InstanceLifecycle:     "spot",
		SpotInstanceRequestId: "sir-1",

		Status:                          vm.StateRunning,
		Instance:                        &ec2.Instance{InstanceId: aws.String("i-round-trip")},
		Reservation:                     &ec2.Reservation{ReservationId: aws.String("r-1")},
		Health:                          vm.InstanceHealthState{CrashCount: 2, LastCrashReason: "oom"},
		LastNode:                        "node-1",
		MetadataServerAddress:           "127.0.0.1:12345",
		ENIId:                           "eni-1",
		ENIMac:                          "aa:bb:cc:dd:ee:01",
		ExtraENIs:                       []vm.ExtraENI{{ENIID: "eni-2"}},
		PublicIP:                        "203.0.113.10",
		PublicIPPool:                    "pool-1",
		PublicIPAllocID:                 "eipalloc-1",
		PublicIPAssocID:                 "eipassoc-1",
		DevMAC:                          "02:00:00:00:00:01",
		MgmtMAC:                         "02:a0:00:00:00:01",
		MgmtIP:                          "10.255.0.5",
		PlacementGroupNode:              "node-2",
		HostfwdPortMap:                  map[int]int{8080: 18080},
		GPUAttachments:                  []gpu.GPUAttachment{{PCIAddress: "0000:81:00.0"}},
		IamInstanceProfileAssociationId: "iip-assoc-1",
		Teardown:                        map[string]string{"volume": "done"},
		TerminatedAt:                    time.Date(2026, 8, 28, 2, 0, 0, 0, time.UTC),
		ShuttingDownAt:                  time.Date(2026, 8, 28, 3, 0, 0, 0, time.UTC),

		QMPClient: &qmp.QMPClient{},
	}
	v.EBSRequests.Requests = []types.EBSRequest{{Name: "vol-1", Boot: true}}
	v.ENIRequests.AvailableSlots = []int{1, 2}
	v.ENIRequests.AttachedByENIID = map[string]int{"eni-1": 0}
	return v
}

func exportedFields(t reflect.Type) []string {
	names := make([]string, 0, t.NumField())
	for f := range t.Fields() {
		if f.IsExported() {
			names = append(names, f.Name)
		}
	}
	return names
}

// containsLock reports whether t embeds a sync lock at any depth. Depth is
// bounded because ec2 SDK types are deep and none of them hold one.
func containsLock(t reflect.Type, depth int) bool {
	if depth > 4 {
		return false
	}
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	if t.PkgPath() == "sync" {
		return true
	}
	for field := range t.Fields() {
		if containsLock(field.Type, depth+1) {
			return true
		}
	}
	return false
}
