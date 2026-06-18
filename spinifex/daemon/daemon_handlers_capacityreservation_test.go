package daemon

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// crIDPattern matches the cr-<17 hex> id minted by GenerateResourceID("cr").
var crIDPattern = regexp.MustCompile(`^cr-[0-9a-f]{17}$`)

// natsRequestAs is natsRequest with an explicit account, for exercising the
// handlers' account scoping (natsRequest always uses testAccountID).
func natsRequestAs(nc *nats.Conn, subject string, data []byte, account string, timeout time.Duration) (*nats.Msg, error) {
	msg := nats.NewMsg(subject)
	msg.Data = data
	msg.Header.Set("X-Account-ID", account)
	return nc.RequestMsg(msg, timeout)
}

// replyErrCode returns the awserror Code on an error reply, or "" if the reply
// is a normal (non-error) payload.
func replyErrCode(t *testing.T, data []byte) string {
	t.Helper()
	var re struct {
		Code string `json:"Code"`
	}
	require.NoError(t, json.Unmarshal(data, &re))
	return re.Code
}

// seedReservation inserts a reservation directly into the node's map, bypassing
// the fit gate so scoping tests can hold several records (the schedulable pool
// only fits one t3.micro under the pinned 4-vCPU test host).
func seedReservation(rm *ResourceManager, id, account, instanceType string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.reservations[id] = &capacityReservation{
		ID:                    id,
		AccountID:             account,
		InstanceType:          instanceType,
		AvailabilityZone:      "ap-southeast-2a",
		TotalInstanceCount:    1,
		VCPUPerInstance:       2,
		MemGBPerInstance:      1.0,
		InstanceMatchCriteria: ec2.InstanceMatchCriteriaOpen,
		Tenancy:               ec2.CapacityReservationTenancyDefault,
		InstancePlatform:      "Linux/UNIX",
		CreateDate:            time.Now().UTC(),
	}
}

// The node-targeted Create handler resolves per-instance compute from the local
// catalog, mints a cr-id, commits the carve-out, and renders the AWS view.
func TestHandleEC2CreateCapacityReservation_RoundTrip(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	instType := getTestInstanceType(t)
	it := daemon.resourceMgr.instanceTypes[instType]
	require.NotNil(t, it, "test instance type missing from catalog")
	wantVCPU := int(instanceTypeVCPUs(it))
	require.GreaterOrEqual(t, daemon.resourceMgr.canAllocate(it, 1000), 1, "host must fit one for the round-trip")

	subject := "ec2.CreateCapacityReservation." + daemon.node
	sub, err := daemon.natsConn.Subscribe(subject, daemon.handleEC2CreateCapacityReservation)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	reqData, _ := json.Marshal(&ec2.CreateCapacityReservationInput{
		InstanceType:     aws.String(instType),
		InstanceCount:    aws.Int64(1),
		AvailabilityZone: aws.String("ap-southeast-2a"),
		InstancePlatform: aws.String("Linux/UNIX"),
	})
	reply, err := natsRequest(daemon.natsConn, subject, reqData, 5*time.Second)
	require.NoError(t, err)
	require.Empty(t, replyErrCode(t, reply.Data), "create should not error: %s", reply.Data)

	var cr ec2.CapacityReservation
	require.NoError(t, json.Unmarshal(reply.Data, &cr))
	assert.Regexp(t, crIDPattern, aws.StringValue(cr.CapacityReservationId))
	assert.Equal(t, testAccountID, aws.StringValue(cr.OwnerId))
	assert.Equal(t, instType, aws.StringValue(cr.InstanceType))
	assert.Equal(t, "ap-southeast-2a", aws.StringValue(cr.AvailabilityZone))
	assert.Equal(t, "Linux/UNIX", aws.StringValue(cr.InstancePlatform))
	assert.Equal(t, int64(1), aws.Int64Value(cr.TotalInstanceCount))
	assert.Equal(t, int64(1), aws.Int64Value(cr.AvailableInstanceCount))
	assert.Equal(t, ec2.CapacityReservationStateActive, aws.StringValue(cr.State))
	assert.Equal(t, ec2.InstanceMatchCriteriaOpen, aws.StringValue(cr.InstanceMatchCriteria), "match criteria defaults to open")
	assert.Equal(t, ec2.CapacityReservationTenancyDefault, aws.StringValue(cr.Tenancy), "tenancy defaults to default")
	assert.Equal(t, ec2.EndDateTypeUnlimited, aws.StringValue(cr.EndDateType))

	daemon.resourceMgr.mu.RLock()
	gotVCPU := daemon.resourceMgr.reservedCRVCPU
	daemon.resourceMgr.mu.RUnlock()
	assert.Equal(t, wantVCPU, gotVCPU, "carve-out committed to the resource manager")
}

// Create rejects an unknown type, a GPU type, and a request larger than the
// node's remaining schedulable pool, leaving no carve-out behind in any case.
func TestHandleEC2CreateCapacityReservation_Rejections(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	// Inject a synthetic GPU type; the catalog otherwise has none.
	daemon.resourceMgr.instanceTypes["g4dn.xlarge"] = &ec2.InstanceTypeInfo{
		InstanceType: aws.String("g4dn.xlarge"),
		VCpuInfo:     &ec2.VCpuInfo{DefaultVCpus: aws.Int64(4)},
		MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(16384)},
		GpuInfo:      &ec2.GpuInfo{Gpus: []*ec2.GpuDeviceInfo{{Name: aws.String("T4"), Count: aws.Int64(1)}}},
	}

	instType := getTestInstanceType(t)
	it := daemon.resourceMgr.instanceTypes[instType]
	require.NotNil(t, it)
	overCount := int64(daemon.resourceMgr.canAllocate(it, 1000) + 1)

	subject := "ec2.CreateCapacityReservation." + daemon.node
	sub, err := daemon.natsConn.Subscribe(subject, daemon.handleEC2CreateCapacityReservation)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	tests := []struct {
		name    string
		input   *ec2.CreateCapacityReservationInput
		wantErr string
	}{
		{
			name: "unknown type",
			input: &ec2.CreateCapacityReservationInput{
				InstanceType: aws.String("nonexistent.type"), InstanceCount: aws.Int64(1),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: awserrors.ErrorInvalidInstanceType,
		},
		{
			name: "gpu type",
			input: &ec2.CreateCapacityReservationInput{
				InstanceType: aws.String("g4dn.xlarge"), InstanceCount: aws.Int64(1),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: awserrors.ErrorInvalidInstanceType,
		},
		{
			name: "exceeds schedulable",
			input: &ec2.CreateCapacityReservationInput{
				InstanceType: aws.String(instType), InstanceCount: aws.Int64(overCount),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: awserrors.ErrorInsufficientInstanceCapacity,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqData, _ := json.Marshal(tt.input)
			reply, err := natsRequest(daemon.natsConn, subject, reqData, 5*time.Second)
			require.NoError(t, err)
			assert.Equal(t, tt.wantErr, replyErrCode(t, reply.Data))
		})
	}

	daemon.resourceMgr.mu.RLock()
	defer daemon.resourceMgr.mu.RUnlock()
	assert.Equal(t, 0, daemon.resourceMgr.reservedCRVCPU, "no carve-out after rejected creates")
	assert.Empty(t, daemon.resourceMgr.reservations)
}

// The fan-out Describe handler returns only the caller's reservations.
func TestHandleEC2DescribeCapacityReservations_Scoping(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	seedReservation(daemon.resourceMgr, "cr-aaaaaaaaaaaaaaaaa", testAccountID, getTestInstanceType(t))
	seedReservation(daemon.resourceMgr, "cr-bbbbbbbbbbbbbbbbb", "999999999999", getTestInstanceType(t))

	sub, err := daemon.natsConn.Subscribe("ec2.DescribeCapacityReservations", daemon.handleEC2DescribeCapacityReservations)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	reqData, _ := json.Marshal(&ec2.DescribeCapacityReservationsInput{})

	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeCapacityReservations", reqData, 5*time.Second)
	require.NoError(t, err)
	var out ec2.DescribeCapacityReservationsOutput
	require.NoError(t, json.Unmarshal(reply.Data, &out))
	require.Len(t, out.CapacityReservations, 1, "only the caller's reservation is returned")
	assert.Equal(t, "cr-aaaaaaaaaaaaaaaaa", aws.StringValue(out.CapacityReservations[0].CapacityReservationId))

	reply, err = natsRequestAs(daemon.natsConn, "ec2.DescribeCapacityReservations", reqData, "111111111111", 5*time.Second)
	require.NoError(t, err)
	var empty ec2.DescribeCapacityReservationsOutput
	require.NoError(t, json.Unmarshal(reply.Data, &empty))
	assert.Empty(t, empty.CapacityReservations, "an account with no reservations sees none")
}

// The broadcast Cancel handler releases the carve-out only for the owning
// account, and every node acks found/not-found so the gateway can disambiguate.
func TestHandleEC2CancelCapacityReservation_ScopingAndAck(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	instType := getTestInstanceType(t)
	it := daemon.resourceMgr.instanceTypes[instType]
	require.NotNil(t, it)
	require.GreaterOrEqual(t, daemon.resourceMgr.canAllocate(it, 1000), 1)
	rec := &capacityReservation{
		ID:                    "cr-ccccccccccccccccc",
		AccountID:             testAccountID,
		InstanceType:          instType,
		AvailabilityZone:      "ap-southeast-2a",
		TotalInstanceCount:    1,
		VCPUPerInstance:       int(instanceTypeVCPUs(it)),
		MemGBPerInstance:      float64(instanceTypeMemoryMiB(it)) / 1024.0,
		InstanceMatchCriteria: ec2.InstanceMatchCriteriaOpen,
		Tenancy:               ec2.CapacityReservationTenancyDefault,
		InstancePlatform:      "Linux/UNIX",
		CreateDate:            time.Now().UTC(),
	}
	require.NoError(t, daemon.resourceMgr.CreateReservation(rec))

	sub, err := daemon.natsConn.Subscribe("ec2.CancelCapacityReservation", daemon.handleEC2CancelCapacityReservation)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	cancel := func(account string) bool {
		reqData, _ := json.Marshal(&ec2.CancelCapacityReservationInput{CapacityReservationId: aws.String(rec.ID)})
		reply, rerr := natsRequestAs(daemon.natsConn, "ec2.CancelCapacityReservation", reqData, account, 5*time.Second)
		require.NoError(t, rerr)
		var out ec2.CancelCapacityReservationOutput
		require.NoError(t, json.Unmarshal(reply.Data, &out))
		return aws.BoolValue(out.Return)
	}

	assert.False(t, cancel("999999999999"), "a foreign account cannot cancel")
	daemon.resourceMgr.mu.RLock()
	heldVCPU := daemon.resourceMgr.reservedCRVCPU
	daemon.resourceMgr.mu.RUnlock()
	assert.Equal(t, rec.VCPUPerInstance, heldVCPU, "carve-out untouched after foreign cancel")

	assert.True(t, cancel(testAccountID), "the owner cancels successfully")
	daemon.resourceMgr.mu.RLock()
	freedVCPU := daemon.resourceMgr.reservedCRVCPU
	daemon.resourceMgr.mu.RUnlock()
	assert.Equal(t, 0, freedVCPU, "carve-out released on cancel")

	assert.False(t, cancel(testAccountID), "a second cancel reports not-found")
}
