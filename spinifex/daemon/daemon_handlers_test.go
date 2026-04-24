package daemon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awss3 "github.com/aws/aws-sdk-go/service/s3"

	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ec2_account "github.com/mulgadc/spinifex/spinifex/handlers/ec2/account"
	handlers_ec2_eigw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eigw"
	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	handlers_ec2_image "github.com/mulgadc/spinifex/spinifex/handlers/ec2/image"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	handlers_ec2_key "github.com/mulgadc/spinifex/spinifex/handlers/ec2/key"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	handlers_ec2_tags "github.com/mulgadc/spinifex/spinifex/handlers/ec2/tags"
	handlers_ec2_volume "github.com/mulgadc/spinifex/spinifex/handlers/ec2/volume"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testAccountID is the default account ID used in daemon tests.
const testAccountID = "123456789012"

// natsRequest sends a NATS request with the X-Account-ID header set.
func natsRequest(nc *nats.Conn, subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	msg := nats.NewMsg(subject)
	msg.Data = data
	msg.Header.Set("X-Account-ID", testAccountID)
	return nc.RequestMsg(msg, timeout)
}

// createFullTestDaemonWithStore creates a test daemon with ALL services initialized
// and returns the shared memory store for seeding test data.
func createFullTestDaemonWithStore(t *testing.T, natsURL string) (*Daemon, *objectstore.MemoryObjectStore) {
	daemon := createTestDaemon(t, natsURL)

	memStore := objectstore.NewMemoryObjectStore()
	cfg := daemon.config

	daemon.keyService = handlers_ec2_key.NewKeyServiceImplWithStore(memStore, cfg.Predastore.Bucket)
	daemon.imageService = handlers_ec2_image.NewImageServiceImplWithStore(memStore, cfg.Predastore.Bucket)
	daemon.volumeService = handlers_ec2_volume.NewVolumeServiceImplWithStore(cfg, memStore, daemon.natsConn)
	daemon.snapshotService = handlers_ec2_snapshot.NewSnapshotServiceImplWithStore(cfg, memStore, daemon.natsConn)
	daemon.tagsService = handlers_ec2_tags.NewTagsServiceImplWithStore(cfg, memStore)
	initAccountServiceForTest(t, daemon)

	return daemon, memStore
}

// createFullTestDaemon creates a test daemon with ALL services initialized (including
// key, image, snapshot, tags, eigw, account) using in-memory object stores.
func createFullTestDaemon(t *testing.T, natsURL string) *Daemon {
	daemon, _ := createFullTestDaemonWithStore(t, natsURL)
	return daemon
}

// createFullTestDaemonWithJetStream creates a test daemon with JetStream KV enabled,
// needed for tests involving state transitions (TransitionState calls WriteState).
func createFullTestDaemonWithJetStream(t *testing.T, natsURL string) *Daemon {
	daemon := createFullTestDaemon(t, natsURL)

	var err error
	daemon.jsManager, err = NewJetStreamManager(daemon.natsConn, 1)
	require.NoError(t, err)
	err = daemon.jsManager.InitKVBucket()
	require.NoError(t, err)
	err = daemon.jsManager.InitTerminatedInstanceBucket()
	require.NoError(t, err)

	return daemon
}

// initAccountServiceForTest initializes a JetStream-backed account service on the daemon
// using an isolated embedded NATS JetStream server per test to avoid shared KV state.
func initAccountServiceForTest(t *testing.T, daemon *Daemon) {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	svc, err := handlers_ec2_account.NewAccountSettingsServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	daemon.accountService = svc
}

// --- handleNATSRequest generic tests ---

type testInput struct {
	Name string `json:"name"`
}

type testOutput struct {
	Greeting string `json:"greeting"`
}

func TestHandleNATSRequest_ValidRequest(t *testing.T) {
	natsURL := sharedNATSURL

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	serviceFn := func(in *testInput, accountID string) (*testOutput, error) {
		return &testOutput{Greeting: "hello " + in.Name}, nil
	}

	sub, err := nc.Subscribe("test.greet", func(msg *nats.Msg) {
		handleNATSRequest(msg, serviceFn)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reqData, _ := json.Marshal(testInput{Name: "world"})
	reply, err := nc.Request("test.greet", reqData, 5*time.Second)
	require.NoError(t, err)

	var out testOutput
	err = json.Unmarshal(reply.Data, &out)
	require.NoError(t, err)
	assert.Equal(t, "hello world", out.Greeting)
}

func TestHandleNATSRequest_MalformedJSON(t *testing.T) {
	natsURL := sharedNATSURL

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	serviceFn := func(in *testInput, accountID string) (*testOutput, error) {
		return &testOutput{Greeting: "hello"}, nil
	}

	sub, err := nc.Subscribe("test.malformed", func(msg *nats.Msg) {
		handleNATSRequest(msg, serviceFn)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := nc.Request("test.malformed", []byte(`{not valid json}`), 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code", "Should contain error code")
}

func TestHandleNATSRequest_ServiceError(t *testing.T) {
	natsURL := sharedNATSURL

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	serviceFn := func(in *testInput, accountID string) (*testOutput, error) {
		return nil, fmt.Errorf("something went wrong")
	}

	sub, err := nc.Subscribe("test.err", func(msg *nats.Msg) {
		handleNATSRequest(msg, serviceFn)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reqData, _ := json.Marshal(testInput{Name: "world"})
	reply, err := nc.Request("test.err", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code", "Should contain error code")
}

// --- Handler wrapper tests (representative set via NATS round-trip) ---

func TestHandleEC2CreateKeyPair_RoundTrip(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.CreateKeyPair", "spinifex-workers", daemon.handleEC2CreateKeyPair)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateKeyPairInput{
		KeyName: aws.String("test-key-001"),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateKeyPair", reqData, 5*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var output ec2.CreateKeyPairOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.Equal(t, "test-key-001", *output.KeyName)
	assert.NotEmpty(t, *output.KeyFingerprint)
	assert.NotEmpty(t, *output.KeyMaterial)
}

func TestHandleEC2CreateTags_RoundTrip(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.CreateTags", "spinifex-workers", daemon.handleEC2CreateTags)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-12345678")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("test-instance")},
		},
	}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.CreateTags", reqData, 5*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var output ec2.CreateTagsOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
}

func TestHandleEC2DescribeImages_RoundTrip(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeImages", "spinifex-workers", daemon.handleEC2DescribeImages)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.DescribeImagesInput{}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.DescribeImages", reqData, 5*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var output ec2.DescribeImagesOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
}

func TestHandleEC2DescribeVolumes_RoundTrip(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeVolumes", "spinifex-workers", daemon.handleEC2DescribeVolumes)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.DescribeVolumesInput{}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.DescribeVolumes", reqData, 5*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var output ec2.DescribeVolumesOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
}

func TestHandleEC2DescribeKeyPairs_RoundTrip(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeKeyPairs", "spinifex-workers", daemon.handleEC2DescribeKeyPairs)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.DescribeKeyPairsInput{}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeKeyPairs", reqData, 5*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var output ec2.DescribeKeyPairsOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
}

// --- handleHealthCheck tests ---

func TestHandleHealthCheck(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createTestDaemon(t, natsURL)

	topic := fmt.Sprintf("spinifex.admin.%s.health", daemon.node)
	sub, err := daemon.natsConn.Subscribe(topic, daemon.handleHealthCheck)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Before ready is set, status should be "starting"
	reply, err := daemon.natsConn.Request(topic, nil, 5*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var resp types.NodeHealthResponse
	err = json.Unmarshal(reply.Data, &resp)
	require.NoError(t, err)

	assert.Equal(t, daemon.node, resp.Node)
	assert.Equal(t, "starting", resp.Status)
	assert.NotEmpty(t, resp.ConfigHash)
	assert.GreaterOrEqual(t, resp.Uptime, int64(0))

	// After marking ready, status should be "running"
	daemon.ready.Store(true)
	reply, err = daemon.natsConn.Request(topic, nil, 5*time.Second)
	require.NoError(t, err)

	err = json.Unmarshal(reply.Data, &resp)
	require.NoError(t, err)
	assert.Equal(t, "running", resp.Status)
}

// --- handleNodeDiscover tests ---

func TestHandleNodeDiscover(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.Subscribe("spinifex.nodes.discover", daemon.handleNodeDiscover)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("spinifex.nodes.discover", nil, 5*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var resp types.NodeDiscoverResponse
	err = json.Unmarshal(reply.Data, &resp)
	require.NoError(t, err)

	assert.Equal(t, daemon.node, resp.Node)
}

// --- handleEC2RunInstances AMI validation tests ---

func TestHandleEC2RunInstances_InvalidAMI(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.RunInstances", "spinifex-workers", daemon.handleEC2RunInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-nonexistent"),
		InstanceType: aws.String(getTestInstanceType(t)),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.RunInstances", reqData, 5*time.Second)
	require.NoError(t, err)

	// Should return InvalidAMIID.NotFound, not ServerInternal
	assert.Contains(t, string(reply.Data), "InvalidAMIID.NotFound")
}

func TestHandleEC2RunInstances_InvalidKeyPair(t *testing.T) {
	natsURL := sharedNATSURL

	daemon, memStore := createFullTestDaemonWithStore(t, natsURL)

	// Seed a valid AMI so AMI validation passes
	seedTestAMI(t, memStore, daemon.config.Predastore.Bucket, "ami-test123")

	sub, err := daemon.natsConn.QueueSubscribe("ec2.RunInstances", "spinifex-workers", daemon.handleEC2RunInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test123"),
		InstanceType: aws.String(getTestInstanceType(t)),
		KeyName:      aws.String("nonexistent-keypair"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.RunInstances", reqData, 5*time.Second)
	require.NoError(t, err)

	// Should return InvalidKeyPair.NotFound, not proceed to launch
	assert.Contains(t, string(reply.Data), "InvalidKeyPair.NotFound")
}

func TestHandleEC2RunInstances_ValidKeyPairPassesValidation(t *testing.T) {
	natsURL := sharedNATSURL

	daemon, memStore := createFullTestDaemonWithStore(t, natsURL)

	// Seed a valid AMI
	seedTestAMI(t, memStore, daemon.config.Predastore.Bucket, "ami-test456")

	// Seed a valid key pair (public key + metadata)
	bucket := daemon.config.Predastore.Bucket
	_, err := memStore.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("keys/" + testAccountID + "/my-key"),
		Body:   strings.NewReader("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest"),
	})
	require.NoError(t, err)

	metadataJSON := `{"KeyPairId":"key-abc123","KeyName":"my-key","KeyFingerprint":"SHA256:test"}`
	_, err = memStore.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("keys/" + testAccountID + "/key-abc123.json"),
		Body:   strings.NewReader(metadataJSON),
	})
	require.NoError(t, err)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.RunInstances", "spinifex-workers", daemon.handleEC2RunInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test456"),
		InstanceType: aws.String(getTestInstanceType(t)),
		KeyName:      aws.String("my-key"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.RunInstances", reqData, 5*time.Second)
	require.NoError(t, err)

	// Should NOT contain InvalidKeyPair.NotFound — key pair validation should pass
	assert.NotContains(t, string(reply.Data), "InvalidKeyPair.NotFound")
}

func TestHandleEC2RunInstances_EmptyKeyNameSkipsValidation(t *testing.T) {
	natsURL := sharedNATSURL

	daemon, memStore := createFullTestDaemonWithStore(t, natsURL)
	seedTestAMI(t, memStore, daemon.config.Predastore.Bucket, "ami-test789")

	sub, err := daemon.natsConn.QueueSubscribe("ec2.RunInstances", "spinifex-workers", daemon.handleEC2RunInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// No KeyName at all — should skip validation
	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test789"),
		InstanceType: aws.String(getTestInstanceType(t)),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.RunInstances", reqData, 5*time.Second)
	require.NoError(t, err)

	// Should NOT contain InvalidKeyPair.NotFound
	assert.NotContains(t, string(reply.Data), "InvalidKeyPair.NotFound")
}

// --- handleEC2RunInstances service-layer error propagation ---

func TestHandleEC2RunInstances_ServiceErrorPropagated(t *testing.T) {
	natsURL := sharedNATSURL

	daemon, memStore := createFullTestDaemonWithStore(t, natsURL)
	seedTestAMI(t, memStore, daemon.config.Predastore.Bucket, "ami-propatest")

	// Override instanceService with one that has an empty instance types map.
	// The resourceMgr still has instance types, so the daemon-level check passes,
	// but RunInstance() will fail with ErrorInvalidInstanceType.
	emptyTypes := map[string]*ec2.InstanceTypeInfo{}
	daemon.instanceService = handlers_ec2_instance.NewInstanceServiceImpl(
		daemon.config, emptyTypes, daemon.natsConn, &daemon.Instances,
		objectstore.NewMemoryObjectStore(),
	)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.RunInstances", "spinifex-workers", daemon.handleEC2RunInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-propatest"),
		InstanceType: aws.String(getTestInstanceType(t)),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.RunInstances", reqData, 5*time.Second)
	require.NoError(t, err)

	// Should propagate the specific AWS error from the service layer,
	// not swallow it into ServerInternal
	assert.Contains(t, string(reply.Data), "InvalidInstanceType")
	assert.NotContains(t, string(reply.Data), "ServerInternal")
}

// --- handleStopOrTerminateInstance tests (JetStream required for TransitionState) ---

func TestHandleEC2Events_StopInstance(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	instanceID := "i-test-stop-001"
	daemon.Instances.VMS[instanceID] = &vm.VM{
		ID:           instanceID,
		InstanceType: getTestInstanceType(t),
		Status:       vm.StateRunning,
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
		AccountID:    testAccountID,
	}

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	cmd := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{StopInstance: true},
	}
	cmdData, _ := json.Marshal(cmd)

	reply, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, reply)

	// Should get immediate {} response
	assert.Equal(t, `{}`, string(reply.Data))

	// State should transition to stopping
	daemon.Instances.Mu.Lock()
	status := daemon.Instances.VMS[instanceID].Status
	daemon.Instances.Mu.Unlock()
	assert.Equal(t, vm.StateStopping, status)
}

func TestHandleEC2Events_TerminateInstance(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	instanceID := "i-test-term-001"
	daemon.Instances.VMS[instanceID] = &vm.VM{
		ID:           instanceID,
		InstanceType: getTestInstanceType(t),
		Status:       vm.StateRunning,
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
		AccountID:    testAccountID,
	}

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	cmd := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{TerminateInstance: true},
	}
	cmdData, _ := json.Marshal(cmd)

	reply, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, reply)

	assert.Equal(t, `{}`, string(reply.Data))

	daemon.Instances.Mu.Lock()
	status := daemon.Instances.VMS[instanceID].Status
	daemon.Instances.Mu.Unlock()
	assert.Equal(t, vm.StateShuttingDown, status)
}

func TestHandleEC2Events_RebootRunningInstance(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createTestDaemon(t, natsURL)

	instanceID := "i-test-reboot-001"
	daemon.Instances.VMS[instanceID] = &vm.VM{
		ID:           instanceID,
		InstanceType: getTestInstanceType(t),
		Status:       vm.StateRunning,
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
		AccountID:    testAccountID,
	}

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	cmd := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{RebootInstance: true},
	}
	cmdData, _ := json.Marshal(cmd)

	reply, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, reply)

	// With nil QMPClient encoder/decoder, SendQMPCommand returns error,
	// so we expect an error response (ServerInternal).
	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")

	// Instance should remain in running state (reboot doesn't change state)
	daemon.Instances.Mu.Lock()
	status := daemon.Instances.VMS[instanceID].Status
	daemon.Instances.Mu.Unlock()
	assert.Equal(t, vm.StateRunning, status)
}

func TestHandleEC2Events_RebootStoppedInstance(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createTestDaemon(t, natsURL)

	instanceID := "i-test-reboot-stopped"
	daemon.Instances.VMS[instanceID] = &vm.VM{
		ID:           instanceID,
		InstanceType: getTestInstanceType(t),
		Status:       vm.StateStopped,
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
		AccountID:    testAccountID,
	}

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	cmd := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{RebootInstance: true},
	}
	cmdData, _ := json.Marshal(cmd)

	reply, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, reply)

	// Should get IncorrectInstanceState error
	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2Events_RebootTerminatedInstance(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createTestDaemon(t, natsURL)

	instanceID := "i-test-reboot-terminated"
	daemon.Instances.VMS[instanceID] = &vm.VM{
		ID:           instanceID,
		InstanceType: getTestInstanceType(t),
		Status:       vm.StateTerminated,
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
		AccountID:    testAccountID,
	}

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	cmd := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{RebootInstance: true},
	}
	cmdData, _ := json.Marshal(cmd)

	reply, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, reply)

	// Should get IncorrectInstanceState error
	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2Events_InstanceNotFound(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.Subscribe("ec2.cmd.i-nonexistent", daemon.handleEC2Events)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	cmd := types.EC2InstanceCommand{
		ID:         "i-nonexistent",
		Attributes: types.EC2CommandAttributes{StopInstance: true},
	}
	cmdData, _ := json.Marshal(cmd)

	reply, err := daemon.natsConn.Request("ec2.cmd.i-nonexistent", cmdData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2Events_MalformedJSON(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.Subscribe("ec2.cmd.test", daemon.handleEC2Events)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("ec2.cmd.test", []byte(`{bad json}`), 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

// --- respondWithVolumeAttachment tests ---

func TestRespondWithVolumeAttachment(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.Subscribe("test.volume.attach", func(msg *nats.Msg) {
		daemon.respondWithVolumeAttachment(msg, "vol-123", "i-456", "/dev/sdf", "attached")
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("test.volume.attach", nil, 5*time.Second)
	require.NoError(t, err)

	var attachment ec2.VolumeAttachment
	err = json.Unmarshal(reply.Data, &attachment)
	require.NoError(t, err)

	assert.Equal(t, "vol-123", *attachment.VolumeId)
	assert.Equal(t, "i-456", *attachment.InstanceId)
	assert.Equal(t, "/dev/sdf", *attachment.Device)
	assert.Equal(t, "attached", *attachment.State)
	assert.NotNil(t, attachment.AttachTime)
	assert.Equal(t, false, *attachment.DeleteOnTermination)
}

// --- handleEC2ModifyVolume tests ---

func TestHandleEC2ModifyVolume_MalformedInput(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.ModifyVolume", "spinifex-workers", daemon.handleEC2ModifyVolume)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("ec2.ModifyVolume", []byte(`{bad}`), 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2ModifyVolume_VolumeNotFound(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.ModifyVolume", "spinifex-workers", daemon.handleEC2ModifyVolume)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-nonexistent"),
		Size:     aws.Int64(16),
	}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.ModifyVolume", reqData, 5*time.Second)
	require.NoError(t, err)

	// Should return an error since the volume doesn't exist
	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

// --- Account settings handler tests ---

func TestHandleEC2GetEbsEncryptionByDefault(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.GetEbsEncryptionByDefault", "spinifex-workers", daemon.handleEC2GetEbsEncryptionByDefault)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetEbsEncryptionByDefaultInput{}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.GetEbsEncryptionByDefault", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.GetEbsEncryptionByDefaultOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.NotNil(t, output.EbsEncryptionByDefault)
}

func TestHandleEC2GetSerialConsoleAccessStatus(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.GetSerialConsoleAccessStatus", "spinifex-workers", daemon.handleEC2GetSerialConsoleAccessStatus)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetSerialConsoleAccessStatusInput{}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.GetSerialConsoleAccessStatus", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.GetSerialConsoleAccessStatusOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.NotNil(t, output.SerialConsoleAccessEnabled)
}

func TestHandleEC2EnableEbsEncryptionByDefault(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.EnableEbsEncryptionByDefault", "spinifex-workers", daemon.handleEC2EnableEbsEncryptionByDefault)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.EnableEbsEncryptionByDefaultInput{}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.EnableEbsEncryptionByDefault", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.EnableEbsEncryptionByDefaultOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.NotNil(t, output.EbsEncryptionByDefault)
	assert.True(t, *output.EbsEncryptionByDefault)
}

func TestHandleEC2DisableEbsEncryptionByDefault(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DisableEbsEncryptionByDefault", "spinifex-workers", daemon.handleEC2DisableEbsEncryptionByDefault)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.DisableEbsEncryptionByDefaultInput{}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DisableEbsEncryptionByDefault", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DisableEbsEncryptionByDefaultOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.NotNil(t, output.EbsEncryptionByDefault)
	assert.False(t, *output.EbsEncryptionByDefault)
}

func TestHandleEC2EnableSerialConsoleAccess(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.EnableSerialConsoleAccess", "spinifex-workers", daemon.handleEC2EnableSerialConsoleAccess)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.EnableSerialConsoleAccessInput{}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.EnableSerialConsoleAccess", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.EnableSerialConsoleAccessOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.NotNil(t, output.SerialConsoleAccessEnabled)
	assert.True(t, *output.SerialConsoleAccessEnabled)
}

func TestHandleEC2DisableSerialConsoleAccess(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DisableSerialConsoleAccess", "spinifex-workers", daemon.handleEC2DisableSerialConsoleAccess)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.DisableSerialConsoleAccessInput{}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DisableSerialConsoleAccess", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DisableSerialConsoleAccessOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.NotNil(t, output.SerialConsoleAccessEnabled)
	assert.False(t, *output.SerialConsoleAccessEnabled)
}

// --- handleEC2CreateImage tests ---

func TestHandleEC2CreateImage_InstanceNotFound(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.Subscribe("ec2.CreateImage", daemon.handleEC2CreateImage)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateImageInput{
		InstanceId: aws.String("i-nonexistent"),
		Name:       aws.String("my-image"),
	}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.CreateImage", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2CreateImage_MissingInstanceId(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.Subscribe("ec2.CreateImage", daemon.handleEC2CreateImage)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateImageInput{
		Name: aws.String("my-image"),
	}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.CreateImage", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2CreateImage_InvalidState(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	// Add an instance in "pending" state (not running or stopped)
	daemon.Instances.Mu.Lock()
	daemon.Instances.VMS["i-pending123"] = &vm.VM{
		ID:        "i-pending123",
		Status:    vm.StatePending,
		AccountID: testAccountID,
		Instance: &ec2.Instance{
			InstanceId: aws.String("i-pending123"),
			ImageId:    aws.String("ami-source"),
			BlockDeviceMappings: []*ec2.InstanceBlockDeviceMapping{
				{
					DeviceName: aws.String("/dev/sda1"),
					Ebs:        &ec2.EbsInstanceBlockDevice{VolumeId: aws.String("vol-root123")},
				},
			},
		},
	}
	daemon.Instances.Mu.Unlock()

	sub, err := daemon.natsConn.Subscribe("ec2.CreateImage", daemon.handleEC2CreateImage)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateImageInput{
		InstanceId: aws.String("i-pending123"),
		Name:       aws.String("my-image"),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateImage", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2CreateImage_NoRootVolume(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	// Add instance with no block device mappings
	daemon.Instances.Mu.Lock()
	daemon.Instances.VMS["i-novol123"] = &vm.VM{
		ID:        "i-novol123",
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Instance: &ec2.Instance{
			InstanceId:          aws.String("i-novol123"),
			ImageId:             aws.String("ami-source"),
			BlockDeviceMappings: []*ec2.InstanceBlockDeviceMapping{},
		},
	}
	daemon.Instances.Mu.Unlock()

	sub, err := daemon.natsConn.Subscribe("ec2.CreateImage", daemon.handleEC2CreateImage)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateImageInput{
		InstanceId: aws.String("i-novol123"),
		Name:       aws.String("my-image"),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateImage", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2CreateImage_MalformedJSON(t *testing.T) {
	natsURL := sharedNATSURL

	daemon := createFullTestDaemon(t, natsURL)

	sub, err := daemon.natsConn.Subscribe("ec2.CreateImage", daemon.handleEC2CreateImage)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("ec2.CreateImage", []byte(`{bad json}`), 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

// --- SetConfigPath test ---

func TestSetConfigPath(t *testing.T) {
	clusterCfg := &config.ClusterConfig{
		Node:  "node-1",
		Nodes: map[string]config.Config{"node-1": {BaseDir: "/tmp"}},
	}
	daemon, err := NewDaemon(clusterCfg)
	require.NoError(t, err)

	assert.Empty(t, daemon.configPath)
	daemon.SetConfigPath("/etc/spinifex/config.toml")
	assert.Equal(t, "/etc/spinifex/config.toml", daemon.configPath)
}

// --- handleEC2StartStoppedInstance tests ---

func TestHandleEC2StartStoppedInstance_MissingInstance(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.start", "spinifex-workers", daemon.handleEC2StartStoppedInstance)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Request to start a non-existent instance
	reqData, _ := json.Marshal(map[string]string{"instance_id": "i-nonexistent"})
	reply, err := daemon.natsConn.Request("ec2.start", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2StartStoppedInstance_MissingInstanceID(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.start", "spinifex-workers", daemon.handleEC2StartStoppedInstance)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Request with empty instance_id
	reqData, _ := json.Marshal(map[string]string{"instance_id": ""})
	reply, err := daemon.natsConn.Request("ec2.start", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2StartStoppedInstance_NotStoppedState(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	// Write an instance in running state to shared KV
	runningVM := &vm.VM{
		ID:           "i-running-shared",
		Status:       vm.StateRunning,
		InstanceType: getTestInstanceType(t),
		AccountID:    testAccountID,
	}
	err := daemon.jsManager.WriteStoppedInstance(runningVM.ID, runningVM)
	require.NoError(t, err)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.start", "spinifex-workers", daemon.handleEC2StartStoppedInstance)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reqData, _ := json.Marshal(map[string]string{"instance_id": "i-running-shared"})
	reply, err := natsRequest(daemon.natsConn, "ec2.start", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")

	// Cleanup
	_ = daemon.jsManager.DeleteStoppedInstance(runningVM.ID)
}

// --- handleEC2DescribeStoppedInstances tests ---

func TestHandleEC2DescribeStoppedInstances_ReturnsStoppedInstances(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	// Write stopped instances to shared KV with full reservation/instance metadata
	stoppedVM := &vm.VM{
		ID:           "i-describe-stopped-001",
		Status:       vm.StateStopped,
		InstanceType: getTestInstanceType(t),
		LastNode:     "node-1",
		AccountID:    testAccountID,
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-test-001"),
			OwnerId:       aws.String("123456789012"),
		},
		Instance: &ec2.Instance{
			InstanceId:   aws.String("i-describe-stopped-001"),
			InstanceType: aws.String(getTestInstanceType(t)),
		},
	}
	err := daemon.jsManager.WriteStoppedInstance(stoppedVM.ID, stoppedVM)
	require.NoError(t, err)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeStoppedInstances", "spinifex-workers", daemon.handleEC2DescribeStoppedInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.DescribeInstancesInput{}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeStoppedInstances", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstancesOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)

	// Find our stopped instance in the output
	found := false
	for _, res := range output.Reservations {
		for _, inst := range res.Instances {
			if inst.InstanceId != nil && *inst.InstanceId == "i-describe-stopped-001" {
				found = true
				assert.Equal(t, "stopped", *inst.State.Name)
				assert.Equal(t, int64(80), *inst.State.Code)
			}
		}
	}
	assert.True(t, found, "Should find stopped instance in DescribeStoppedInstances output")

	// Cleanup
	_ = daemon.jsManager.DeleteStoppedInstance(stoppedVM.ID)
}

func TestHandleEC2DescribeStoppedInstances_WithFilter(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	// Write two stopped instances
	for _, id := range []string{"i-filter-001", "i-filter-002"} {
		v := &vm.VM{
			ID:        id,
			Status:    vm.StateStopped,
			LastNode:  "node-1",
			AccountID: testAccountID,
			Reservation: &ec2.Reservation{
				ReservationId: aws.String("r-filter"),
				OwnerId:       aws.String("123456789012"),
			},
			Instance: &ec2.Instance{
				InstanceId: aws.String(id),
			},
		}
		err := daemon.jsManager.WriteStoppedInstance(id, v)
		require.NoError(t, err)
	}

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeStoppedInstances", "spinifex-workers", daemon.handleEC2DescribeStoppedInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Filter for only one instance
	input := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String("i-filter-001")},
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeStoppedInstances", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstancesOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)

	// Count matching instances
	var matchCount int
	for _, res := range output.Reservations {
		for _, inst := range res.Instances {
			if inst.InstanceId != nil && *inst.InstanceId == "i-filter-001" {
				matchCount++
			}
			// Should NOT contain i-filter-002
			if inst.InstanceId != nil && *inst.InstanceId == "i-filter-002" {
				t.Error("Should not contain i-filter-002 when filtering for i-filter-001")
			}
		}
	}
	assert.Equal(t, 1, matchCount, "Should find exactly one filtered instance")

	// Cleanup
	_ = daemon.jsManager.DeleteStoppedInstance("i-filter-001")
	_ = daemon.jsManager.DeleteStoppedInstance("i-filter-002")
}

// --- handleEC2TerminateStoppedInstance tests ---

func TestHandleEC2TerminateStoppedInstance_MissingInstanceID(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.terminate", "spinifex-workers", daemon.handleEC2TerminateStoppedInstance)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reqData, _ := json.Marshal(map[string]string{"instance_id": ""})
	reply, err := daemon.natsConn.Request("ec2.terminate", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2TerminateStoppedInstance_MissingInstance(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.terminate", "spinifex-workers", daemon.handleEC2TerminateStoppedInstance)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reqData, _ := json.Marshal(map[string]string{"instance_id": "i-nonexistent"})
	reply, err := daemon.natsConn.Request("ec2.terminate", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2TerminateStoppedInstance_NotStoppedState(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	// Write an instance in running state to shared KV
	runningVM := &vm.VM{
		ID:           "i-term-running",
		Status:       vm.StateRunning,
		InstanceType: getTestInstanceType(t),
		AccountID:    testAccountID,
	}
	err := daemon.jsManager.WriteStoppedInstance(runningVM.ID, runningVM)
	require.NoError(t, err)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.terminate", "spinifex-workers", daemon.handleEC2TerminateStoppedInstance)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reqData, _ := json.Marshal(map[string]string{"instance_id": "i-term-running"})
	reply, err := natsRequest(daemon.natsConn, "ec2.terminate", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")

	// Cleanup
	_ = daemon.jsManager.DeleteStoppedInstance(runningVM.ID)
}

func TestHandleEC2TerminateStoppedInstance_Success(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	// Write a stopped instance to shared KV
	stoppedVM := &vm.VM{
		ID:           "i-term-stopped-001",
		Status:       vm.StateStopped,
		InstanceType: getTestInstanceType(t),
		LastNode:     "node-1",
		AccountID:    testAccountID,
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-term-001"),
			OwnerId:       aws.String("123456789012"),
		},
		Instance: &ec2.Instance{
			InstanceId:   aws.String("i-term-stopped-001"),
			InstanceType: aws.String(getTestInstanceType(t)),
		},
	}
	err := daemon.jsManager.WriteStoppedInstance(stoppedVM.ID, stoppedVM)
	require.NoError(t, err)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.terminate", "spinifex-workers", daemon.handleEC2TerminateStoppedInstance)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reqData, _ := json.Marshal(map[string]string{"instance_id": "i-term-stopped-001"})
	reply, err := natsRequest(daemon.natsConn, "ec2.terminate", reqData, 5*time.Second)
	require.NoError(t, err)

	var resp map[string]string
	err = json.Unmarshal(reply.Data, &resp)
	require.NoError(t, err)
	assert.Equal(t, "terminated", resp["status"])
	assert.Equal(t, "i-term-stopped-001", resp["instanceId"])

	// Verify instance was removed from shared KV
	loaded, err := daemon.jsManager.LoadStoppedInstance("i-term-stopped-001")
	require.NoError(t, err)
	assert.Nil(t, loaded, "Instance should be removed from shared KV after termination")
}

func TestHandleEC2GetConsoleOutput(t *testing.T) {
	natsURL := sharedNATSURL
	daemon := createFullTestDaemon(t, natsURL)

	instanceID := "i-console-test-001"

	// Create a temp console log file
	tmpDir := t.TempDir()
	logPath := tmpDir + "/console-" + instanceID + ".log"
	require.NoError(t, os.WriteFile(logPath, []byte("Hello from serial console\nBoot complete."), 0644))

	// Add an instance with console log path
	daemon.Instances.Mu.Lock()
	daemon.Instances.VMS[instanceID] = &vm.VM{
		ID:        instanceID,
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Config: vm.Config{
			ConsoleLogPath: logPath,
		},
	}
	daemon.Instances.Mu.Unlock()

	topic := fmt.Sprintf("ec2.%s.GetConsoleOutput", instanceID)
	sub, err := daemon.natsConn.Subscribe(topic, daemon.handleEC2GetConsoleOutput)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetConsoleOutputInput{
		InstanceId: aws.String(instanceID),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, topic, reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.GetConsoleOutputOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.Equal(t, instanceID, *output.InstanceId)
	assert.NotNil(t, output.Output)
	assert.NotEmpty(t, *output.Output)
	assert.NotNil(t, output.Timestamp)

	// Decode base64 output and verify content
	decoded, err := base64.StdEncoding.DecodeString(*output.Output)
	require.NoError(t, err)
	assert.Contains(t, string(decoded), "Boot complete.")
}

func TestHandleEC2GetConsoleOutput_EmptyLog(t *testing.T) {
	natsURL := sharedNATSURL
	daemon := createFullTestDaemon(t, natsURL)

	instanceID := "i-console-empty-001"

	// Instance exists but no log file yet
	daemon.Instances.Mu.Lock()
	daemon.Instances.VMS[instanceID] = &vm.VM{
		ID:        instanceID,
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Config: vm.Config{
			ConsoleLogPath: "/nonexistent/console.log",
		},
	}
	daemon.Instances.Mu.Unlock()

	topic := fmt.Sprintf("ec2.%s.GetConsoleOutput", instanceID)
	sub, err := daemon.natsConn.Subscribe(topic, daemon.handleEC2GetConsoleOutput)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetConsoleOutputInput{
		InstanceId: aws.String(instanceID),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, topic, reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.GetConsoleOutputOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.Equal(t, instanceID, *output.InstanceId)
	assert.NotNil(t, output.Output)
	assert.Empty(t, *output.Output)
}

func TestHandleEC2GetConsoleOutput_NotFound(t *testing.T) {
	natsURL := sharedNATSURL
	daemon := createFullTestDaemon(t, natsURL)

	instanceID := "i-nonexistent-console"
	topic := fmt.Sprintf("ec2.%s.GetConsoleOutput", instanceID)
	sub, err := daemon.natsConn.Subscribe(topic, daemon.handleEC2GetConsoleOutput)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetConsoleOutputInput{
		InstanceId: aws.String(instanceID),
	}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request(topic, reqData, 5*time.Second)
	require.NoError(t, err)

	// Should get an error response (instance not found)
	assert.Contains(t, string(reply.Data), "InvalidInstanceID.NotFound")
}

// TestAttachVolume_ZoneMismatch verifies that attaching a volume in a different AZ
// returns InvalidVolume.ZoneMismatch instead of proceeding.
func TestAttachVolume_ZoneMismatch(t *testing.T) {
	natsURL := sharedNATSURL
	daemon, store := createFullTestDaemonWithStore(t, natsURL)

	// Set the daemon's AZ
	daemon.config.AZ = "us-east-1a"

	instanceID := "i-test-az-mismatch"
	volumeID := "vol-az-mismatch"

	// Create a running instance
	instance := &vm.VM{
		ID:           instanceID,
		InstanceType: getTestInstanceType(t),
		Status:       vm.StateRunning,
		AccountID:    testAccountID,
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	daemon.Instances.VMS[instanceID] = instance

	// Create a volume in a different AZ
	wrapper := struct {
		VolumeConfig viperblock.VolumeConfig `json:"VolumeConfig"`
	}{
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				VolumeID:         volumeID,
				SizeGiB:          10,
				State:            "available",
				AvailabilityZone: "us-west-2a",
			},
		},
	}
	data, _ := json.Marshal(wrapper)
	store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})

	// Subscribe handler
	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			AttachVolume: true,
		},
		AttachVolumeData: &types.AttachVolumeData{
			VolumeID: volumeID,
		},
	}
	cmdData, _ := json.Marshal(command)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "InvalidVolume.ZoneMismatch")
}

// --- handleEC2ModifyInstanceAttribute tests ---

func TestHandleEC2ModifyInstanceAttribute_ChangeInstanceType(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers", daemon.handleEC2ModifyInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-modify-type-001"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateStopped,
		InstanceType: "t3.micro",
		AccountID:    testAccountID,
		Config:       vm.Config{InstanceType: "t3.micro"},
		Instance: &ec2.Instance{
			InstanceId:   aws.String(instanceID),
			InstanceType: aws.String("t3.micro"),
		},
	}
	err = daemon.jsManager.WriteStoppedInstance(instanceID, instance)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(instanceID) })

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(instanceID),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.ModifyInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, `{}`, string(reply.Data))

	updated, err := daemon.jsManager.LoadStoppedInstance(instanceID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, "t3.medium", updated.InstanceType)
	assert.Equal(t, "t3.medium", updated.Config.InstanceType)
	assert.Equal(t, "t3.medium", *updated.Instance.InstanceType)
}

func TestHandleEC2ModifyInstanceAttribute_ChangeUserData(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers", daemon.handleEC2ModifyInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-modify-ud-001"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateStopped,
		InstanceType: "t3.micro",
		AccountID:    testAccountID,
		UserData:     "old data",
		RunInstancesInput: &ec2.RunInstancesInput{
			UserData: aws.String("b2xkIGRhdGE="),
		},
		Instance: &ec2.Instance{
			InstanceId: aws.String(instanceID),
		},
	}
	err = daemon.jsManager.WriteStoppedInstance(instanceID, instance)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(instanceID) })

	// Value holds decoded bytes (the gateway query parser decodes base64 from the CLI,
	// then json.Marshal/Unmarshal round-trips []byte through base64 transparently)
	newContent := "#!/bin/bash"
	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		UserData:   &ec2.BlobAttributeValue{Value: []byte(newContent)},
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.ModifyInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, `{}`, string(reply.Data))

	updated, err := daemon.jsManager.LoadStoppedInstance(instanceID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, newContent, updated.UserData)
	assert.Equal(t, "IyEvYmluL2Jhc2g=", *updated.RunInstancesInput.UserData)
}

func TestHandleEC2ModifyInstanceAttribute_SourceDestCheck(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers", daemon.handleEC2ModifyInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// SourceDestCheck is a no-op that succeeds without requiring a stopped instance
	// in KV — Terraform sends this on running instances right after creation.
	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:      aws.String("i-modify-sdc-001"),
		SourceDestCheck: &ec2.AttributeBooleanValue{Value: aws.Bool(false)},
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.ModifyInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, `{}`, string(reply.Data))
}

func TestHandleEC2ModifyInstanceAttribute_InstanceNotFound(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers", daemon.handleEC2ModifyInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-nonexistent"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.ModifyInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Equal(t, "InvalidInstanceID.NotFound", errResp["Code"])
}

func TestHandleEC2ModifyInstanceAttribute_NotStopped(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers", daemon.handleEC2ModifyInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-modify-running-001"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		InstanceType: "t3.micro",
		AccountID:    testAccountID,
	}
	err = daemon.jsManager.WriteStoppedInstance(instanceID, instance)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(instanceID) })

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(instanceID),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.ModifyInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Equal(t, "IncorrectInstanceState", errResp["Code"])
}

func TestHandleEC2ModifyInstanceAttribute_ClearsStateReason(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers", daemon.handleEC2ModifyInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-modify-recovery-001"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateStopped,
		InstanceType: "m7i.small",
		AccountID:    testAccountID,
		Config:       vm.Config{InstanceType: "m7i.small"},
		Instance: &ec2.Instance{
			InstanceId:   aws.String(instanceID),
			InstanceType: aws.String("m7i.small"),
			StateReason: &ec2.StateReason{
				Code:    aws.String("Server.InsufficientInstanceCapacity"),
				Message: aws.String("Instance type not available on any node"),
			},
		},
	}
	err = daemon.jsManager.WriteStoppedInstance(instanceID, instance)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(instanceID) })

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(instanceID),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.micro")},
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.ModifyInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, `{}`, string(reply.Data))

	updated, err := daemon.jsManager.LoadStoppedInstance(instanceID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, "t3.micro", updated.InstanceType)
	assert.Equal(t, "t3.micro", updated.Config.InstanceType)
	assert.Equal(t, "t3.micro", *updated.Instance.InstanceType)
	assert.Nil(t, updated.Instance.StateReason)
}

func TestHandleEC2ModifyInstanceAttribute_InvalidTypeAccepted(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers", daemon.handleEC2ModifyInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-modify-nonsense-001"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateStopped,
		InstanceType: "t3.micro",
		AccountID:    testAccountID,
		Config:       vm.Config{InstanceType: "t3.micro"},
		Instance: &ec2.Instance{
			InstanceId:   aws.String(instanceID),
			InstanceType: aws.String("t3.micro"),
		},
	}
	err = daemon.jsManager.WriteStoppedInstance(instanceID, instance)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(instanceID) })

	// z99.mega is nonsense — modify does not pre-validate, matching AWS behavior
	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(instanceID),
		InstanceType: &ec2.AttributeValue{Value: aws.String("z99.mega")},
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.ModifyInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, `{}`, string(reply.Data))

	updated, err := daemon.jsManager.LoadStoppedInstance(instanceID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, "z99.mega", updated.InstanceType)
	assert.Equal(t, "z99.mega", updated.Config.InstanceType)
	assert.Equal(t, "z99.mega", *updated.Instance.InstanceType)
}

func TestHandleEC2ModifyInstanceAttribute_MissingInstanceID(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers", daemon.handleEC2ModifyInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.ModifyInstanceAttributeInput{}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.ModifyInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Equal(t, "MissingParameter", errResp["Code"])
}

func TestHandleEC2ModifyInstanceAttribute_InvalidJSON(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers", daemon.handleEC2ModifyInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("ec2.ModifyInstanceAttribute", []byte(`{invalid`), 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Equal(t, "ServerInternal", errResp["Code"])
}

// --- DescribeInstanceAttribute daemon tests ---

func TestHandleEC2DescribeInstanceAttribute_RunningInstance_InstanceType(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeInstanceAttribute", "spinifex-workers", daemon.handleEC2DescribeInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-describe-run-001"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		InstanceType: "t3.micro",
		AccountID:    testAccountID,
		Instance: &ec2.Instance{
			InstanceId:   aws.String(instanceID),
			InstanceType: aws.String("t3.micro"),
		},
	}

	daemon.Instances.Mu.Lock()
	daemon.Instances.VMS[instanceID] = instance
	daemon.Instances.Mu.Unlock()
	t.Cleanup(func() {
		daemon.Instances.Mu.Lock()
		delete(daemon.Instances.VMS, instanceID)
		daemon.Instances.Mu.Unlock()
	})

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstanceAttributeOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.Equal(t, instanceID, *output.InstanceId)
	require.NotNil(t, output.InstanceType)
	assert.Equal(t, "t3.micro", *output.InstanceType.Value)
}

func TestHandleEC2DescribeInstanceAttribute_StoppedInstance_InstanceType(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeInstanceAttribute", "spinifex-workers", daemon.handleEC2DescribeInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-describe-stop-001"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateStopped,
		InstanceType: "t3.medium",
		AccountID:    testAccountID,
		Instance: &ec2.Instance{
			InstanceId:   aws.String(instanceID),
			InstanceType: aws.String("t3.medium"),
		},
	}
	err = daemon.jsManager.WriteStoppedInstance(instanceID, instance)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(instanceID) })

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstanceAttributeOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.Equal(t, instanceID, *output.InstanceId)
	require.NotNil(t, output.InstanceType)
	assert.Equal(t, "t3.medium", *output.InstanceType.Value)
}

func TestHandleEC2DescribeInstanceAttribute_UserData(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeInstanceAttribute", "spinifex-workers", daemon.handleEC2DescribeInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-describe-ud-001"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateStopped,
		InstanceType: "t3.micro",
		AccountID:    testAccountID,
		UserData:     "#!/bin/bash",
		Instance: &ec2.Instance{
			InstanceId: aws.String(instanceID),
		},
	}
	err = daemon.jsManager.WriteStoppedInstance(instanceID, instance)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(instanceID) })

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Attribute:  aws.String(ec2.InstanceAttributeNameUserData),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstanceAttributeOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	require.NotNil(t, output.UserData)
	assert.Equal(t, "#!/bin/bash", *output.UserData.Value)
}

func TestHandleEC2DescribeInstanceAttribute_DefaultAttribute_DisableApiTermination(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeInstanceAttribute", "spinifex-workers", daemon.handleEC2DescribeInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-describe-def-001"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateStopped,
		InstanceType: "t3.micro",
		AccountID:    testAccountID,
		Instance: &ec2.Instance{
			InstanceId: aws.String(instanceID),
		},
	}
	err = daemon.jsManager.WriteStoppedInstance(instanceID, instance)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(instanceID) })

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Attribute:  aws.String(ec2.InstanceAttributeNameDisableApiTermination),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstanceAttributeOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	require.NotNil(t, output.DisableApiTermination)
	assert.Equal(t, false, *output.DisableApiTermination.Value)
}

func TestHandleEC2DescribeInstanceAttribute_DefaultAttribute_ShutdownBehavior(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeInstanceAttribute", "spinifex-workers", daemon.handleEC2DescribeInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-describe-shut-001"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateStopped,
		InstanceType: "t3.micro",
		AccountID:    testAccountID,
		Instance: &ec2.Instance{
			InstanceId: aws.String(instanceID),
		},
	}
	err = daemon.jsManager.WriteStoppedInstance(instanceID, instance)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(instanceID) })

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceInitiatedShutdownBehavior),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstanceAttributeOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	require.NotNil(t, output.InstanceInitiatedShutdownBehavior)
	assert.Equal(t, "stop", *output.InstanceInitiatedShutdownBehavior.Value)
}

func TestHandleEC2DescribeInstanceAttribute_GroupSet_WithSecurityGroups(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeInstanceAttribute", "spinifex-workers", daemon.handleEC2DescribeInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-describe-gs-001"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		InstanceType: "t3.micro",
		AccountID:    testAccountID,
		Instance: &ec2.Instance{
			InstanceId: aws.String(instanceID),
			SecurityGroups: []*ec2.GroupIdentifier{
				{GroupId: aws.String("sg-111"), GroupName: aws.String("default")},
				{GroupId: aws.String("sg-222"), GroupName: aws.String("web")},
			},
		},
	}

	daemon.Instances.Mu.Lock()
	daemon.Instances.VMS[instanceID] = instance
	daemon.Instances.Mu.Unlock()
	t.Cleanup(func() {
		daemon.Instances.Mu.Lock()
		delete(daemon.Instances.VMS, instanceID)
		daemon.Instances.Mu.Unlock()
	})

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Attribute:  aws.String(ec2.InstanceAttributeNameGroupSet),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstanceAttributeOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.Equal(t, instanceID, *output.InstanceId)
	require.Len(t, output.Groups, 2)
	assert.Equal(t, "sg-111", *output.Groups[0].GroupId)
	assert.Equal(t, "sg-222", *output.Groups[1].GroupId)
}

func TestHandleEC2DescribeInstanceAttribute_GroupSet_NilInstance(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeInstanceAttribute", "spinifex-workers", daemon.handleEC2DescribeInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-describe-gs-nil-001"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateStopped,
		InstanceType: "t3.micro",
		AccountID:    testAccountID,
		Instance:     nil,
	}
	err = daemon.jsManager.WriteStoppedInstance(instanceID, instance)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(instanceID) })

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Attribute:  aws.String(ec2.InstanceAttributeNameGroupSet),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstanceAttributeOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.Equal(t, instanceID, *output.InstanceId)
	require.NotNil(t, output.Groups, "Groups should be empty slice, not nil")
	assert.Empty(t, output.Groups)
}

func TestHandleEC2DescribeInstanceAttribute_InstanceNotFound(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeInstanceAttribute", "spinifex-workers", daemon.handleEC2DescribeInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-nonexistent"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Equal(t, "InvalidInstanceID.NotFound", errResp["Code"])
}

func TestHandleEC2DescribeInstanceAttribute_UnsupportedAttribute(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeInstanceAttribute", "spinifex-workers", daemon.handleEC2DescribeInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instanceID := "i-describe-unsup-001"
	instance := &vm.VM{
		ID:        instanceID,
		Status:    vm.StateStopped,
		AccountID: testAccountID,
		Instance: &ec2.Instance{
			InstanceId: aws.String(instanceID),
		},
	}
	err = daemon.jsManager.WriteStoppedInstance(instanceID, instance)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(instanceID) })

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Attribute:  aws.String(ec2.InstanceAttributeNameBlockDeviceMapping),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstanceAttribute", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Equal(t, "InvalidParameterValue", errResp["Code"])
}

func TestHandleEC2DescribeInstanceAttribute_InvalidJSON(t *testing.T) {
	natsURL := sharedJSNATSURL

	daemon := createFullTestDaemonWithJetStream(t, natsURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeInstanceAttribute", "spinifex-workers", daemon.handleEC2DescribeInstanceAttribute)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("ec2.DescribeInstanceAttribute", []byte(`{invalid`), 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Equal(t, "ServerInternal", errResp["Code"])
}

// --- Delegate handler round-trip tests (table-driven) ---
// Each of these handlers is a single line delegating to handleNATSRequest.
// This test verifies the wiring is correct by sending a NATS request and
// checking for a valid JSON response.

func TestDelegateHandlers_RoundTrip(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	tests := []struct {
		name    string
		topic   string
		handler func(*nats.Msg)
		input   any
	}{
		{
			"DeleteKeyPair",
			"ec2.test.DeleteKeyPair",
			daemon.handleEC2DeleteKeyPair,
			&ec2.DeleteKeyPairInput{KeyName: aws.String("nonexistent-key")},
		},
		{
			"ImportKeyPair",
			"ec2.test.ImportKeyPair",
			daemon.handleEC2ImportKeyPair,
			&ec2.ImportKeyPairInput{
				KeyName:           aws.String("imported-key"),
				PublicKeyMaterial: []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@test"),
			},
		},
		{
			"CreateVolume",
			"ec2.test.CreateVolume",
			daemon.handleEC2CreateVolume,
			&ec2.CreateVolumeInput{
				AvailabilityZone: aws.String("us-east-1a"),
				Size:             aws.Int64(10),
			},
		},
		{
			"DescribeVolumeStatus",
			"ec2.test.DescribeVolumeStatus",
			daemon.handleEC2DescribeVolumeStatus,
			&ec2.DescribeVolumeStatusInput{},
		},
		{
			"DeleteVolume",
			"ec2.test.DeleteVolume",
			daemon.handleEC2DeleteVolume,
			&ec2.DeleteVolumeInput{VolumeId: aws.String("vol-nonexistent")},
		},
		{
			"CreateSnapshot",
			"ec2.test.CreateSnapshot",
			daemon.handleEC2CreateSnapshot,
			&ec2.CreateSnapshotInput{VolumeId: aws.String("vol-nonexistent")},
		},
		{
			"DescribeSnapshots",
			"ec2.test.DescribeSnapshots",
			daemon.handleEC2DescribeSnapshots,
			&ec2.DescribeSnapshotsInput{},
		},
		{
			"DeleteSnapshot",
			"ec2.test.DeleteSnapshot",
			daemon.handleEC2DeleteSnapshot,
			&ec2.DeleteSnapshotInput{SnapshotId: aws.String("snap-nonexistent")},
		},
		{
			"CopySnapshot",
			"ec2.test.CopySnapshot",
			daemon.handleEC2CopySnapshot,
			&ec2.CopySnapshotInput{
				SourceRegion:     aws.String("us-east-1"),
				SourceSnapshotId: aws.String("snap-nonexistent"),
			},
		},
		{
			"DeleteTags",
			"ec2.test.DeleteTags",
			daemon.handleEC2DeleteTags,
			&ec2.DeleteTagsInput{
				Resources: []*string{aws.String("i-12345678")},
			},
		},
		{
			"DescribeTags",
			"ec2.test.DescribeTags",
			daemon.handleEC2DescribeTags,
			&ec2.DescribeTagsInput{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, err := daemon.natsConn.QueueSubscribe(tt.topic, "spinifex-workers", tt.handler)
			require.NoError(t, err)
			defer sub.Unsubscribe()

			reqData, err := json.Marshal(tt.input)
			require.NoError(t, err)

			reply, err := natsRequest(daemon.natsConn, tt.topic, reqData, 5*time.Second)
			require.NoError(t, err)
			require.NotNil(t, reply)

			// Verify response is valid JSON (either success output or error response)
			var resp json.RawMessage
			err = json.Unmarshal(reply.Data, &resp)
			require.NoError(t, err, "response should be valid JSON: %s", string(reply.Data))
		})
	}
}

// --- daemonIP tests ---

func TestDaemonIP(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		expected string
	}{
		{"HostPort", "10.0.0.1:4432", "10.0.0.1"},
		{"HostOnly", "myhost", "myhost"},
		{"IPv6", "[::1]:4432", "::1"},
		{"EmptyString", "", ""},
		{"HostPortZero", "0.0.0.0:0", "127.0.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			daemon := createTestDaemon(t, sharedNATSURL)
			daemon.config.Daemon.Host = tt.host
			assert.Equal(t, tt.expected, daemon.daemonIP())
		})
	}
}

// --- handleNodeStatus tests ---

func TestHandleNodeStatus(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	// Set identifiable config values
	daemon.config.Region = "us-west-2"
	daemon.config.AZ = "us-west-2a"
	daemon.config.Daemon.Host = "10.0.0.5:4432"

	// Add some VMs (2 running, 1 stopped — only running counted)
	daemon.Instances.Mu.Lock()
	daemon.Instances.VMS["i-run-1"] = &vm.VM{ID: "i-run-1", Status: vm.StateRunning}
	daemon.Instances.VMS["i-run-2"] = &vm.VM{ID: "i-run-2", Status: vm.StateRunning}
	daemon.Instances.VMS["i-stop-1"] = &vm.VM{ID: "i-stop-1", Status: vm.StateStopped}
	daemon.Instances.Mu.Unlock()

	sub, err := daemon.natsConn.Subscribe("spinifex.node.status.test", daemon.handleNodeStatus)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("spinifex.node.status.test", nil, 5*time.Second)
	require.NoError(t, err)

	var resp types.NodeStatusResponse
	err = json.Unmarshal(reply.Data, &resp)
	require.NoError(t, err)

	assert.Equal(t, "node-1", resp.Node)
	assert.Equal(t, "Ready", resp.Status)
	assert.Equal(t, "10.0.0.5", resp.Host)
	assert.Equal(t, "us-west-2", resp.Region)
	assert.Equal(t, "us-west-2a", resp.AZ)
	assert.GreaterOrEqual(t, resp.Uptime, int64(0))
	assert.Equal(t, 2, resp.VMCount)
	assert.Greater(t, resp.TotalVCPU, 0)
	assert.Greater(t, resp.TotalMemGB, 0.0)
}

func TestHandleNodeStatus_NoVMs(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	daemon.config.Daemon.Host = "192.168.1.1:4432"

	sub, err := daemon.natsConn.Subscribe("spinifex.node.status.empty", daemon.handleNodeStatus)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("spinifex.node.status.empty", nil, 5*time.Second)
	require.NoError(t, err)

	var resp types.NodeStatusResponse
	err = json.Unmarshal(reply.Data, &resp)
	require.NoError(t, err)

	assert.Equal(t, 0, resp.VMCount)
	assert.Equal(t, "Ready", resp.Status)
}

// --- handleNodeVMs tests ---

func TestHandleNodeVMs(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	daemon.config.Daemon.Host = "10.0.0.5:4432"

	instanceType := getTestInstanceType(t)
	launchTime := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)

	daemon.Instances.Mu.Lock()
	daemon.Instances.VMS["i-vm-1"] = &vm.VM{
		ID:           "i-vm-1",
		Status:       vm.StateRunning,
		InstanceType: instanceType,
		Instance: &ec2.Instance{
			LaunchTime: &launchTime,
		},
	}
	daemon.Instances.VMS["i-vm-2"] = &vm.VM{
		ID:           "i-vm-2",
		Status:       vm.StateStopped,
		InstanceType: instanceType,
		Instance:     nil, // no launch time
	}
	daemon.Instances.Mu.Unlock()

	sub, err := daemon.natsConn.Subscribe("spinifex.node.vms.test", daemon.handleNodeVMs)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("spinifex.node.vms.test", nil, 5*time.Second)
	require.NoError(t, err)

	var resp types.NodeVMsResponse
	err = json.Unmarshal(reply.Data, &resp)
	require.NoError(t, err)

	assert.Equal(t, "node-1", resp.Node)
	assert.Equal(t, "10.0.0.5", resp.Host)
	assert.Len(t, resp.VMs, 2)

	// Build a lookup by instance ID
	vmsByID := make(map[string]types.VMInfo)
	for _, v := range resp.VMs {
		vmsByID[v.InstanceID] = v
	}

	vm1 := vmsByID["i-vm-1"]
	assert.Equal(t, "running", vm1.Status)
	assert.Equal(t, instanceType, vm1.InstanceType)
	assert.Greater(t, vm1.VCPU, 0)
	assert.Greater(t, vm1.MemoryGB, 0.0)
	assert.Equal(t, launchTime.Unix(), vm1.LaunchTime)

	vm2 := vmsByID["i-vm-2"]
	assert.Equal(t, "stopped", vm2.Status)
	assert.Equal(t, int64(0), vm2.LaunchTime)
}

func TestHandleNodeVMs_Empty(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	daemon.config.Daemon.Host = "10.0.0.5:4432"

	sub, err := daemon.natsConn.Subscribe("spinifex.node.vms.empty", daemon.handleNodeVMs)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("spinifex.node.vms.empty", nil, 5*time.Second)
	require.NoError(t, err)

	var resp types.NodeVMsResponse
	err = json.Unmarshal(reply.Data, &resp)
	require.NoError(t, err)

	assert.Equal(t, "node-1", resp.Node)
	assert.Empty(t, resp.VMs)
}

func TestHandleNodeVMs_UnknownInstanceType(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	daemon.config.Daemon.Host = "10.0.0.5:4432"

	daemon.Instances.Mu.Lock()
	daemon.Instances.VMS["i-vm-unknown"] = &vm.VM{
		ID:           "i-vm-unknown",
		Status:       vm.StateRunning,
		InstanceType: "z99.mega", // not in instanceTypes map
	}
	daemon.Instances.Mu.Unlock()

	sub, err := daemon.natsConn.Subscribe("spinifex.node.vms.unknown", daemon.handleNodeVMs)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("spinifex.node.vms.unknown", nil, 5*time.Second)
	require.NoError(t, err)

	var resp types.NodeVMsResponse
	err = json.Unmarshal(reply.Data, &resp)
	require.NoError(t, err)

	assert.Len(t, resp.VMs, 1)
	assert.Equal(t, 0, resp.VMs[0].VCPU)
	assert.Equal(t, 0.0, resp.VMs[0].MemoryGB)
}

// --- VPC/IGW daemon handler round-trip tests ---

// createVPCTestDaemon creates a test daemon with VPC and IGW services initialized
// using an isolated JetStream server for KV storage.
func createVPCTestDaemon(t *testing.T) *Daemon {
	t.Helper()

	daemon := createTestDaemon(t, sharedNATSURL)

	ns, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	vpcSvc, err := handlers_ec2_vpc.NewVPCServiceImplWithNATS(daemon.config, nc)
	require.NoError(t, err)
	daemon.vpcService = vpcSvc

	igwSvc, err := handlers_ec2_igw.NewIGWServiceImplWithNATS(daemon.config, nc)
	require.NoError(t, err)
	daemon.igwService = igwSvc

	return daemon
}

func TestDelegateHandlers_VPC(t *testing.T) {
	daemon := createVPCTestDaemon(t)

	tests := []struct {
		name    string
		topic   string
		handler func(*nats.Msg)
		input   any
	}{
		{
			"CreateVpc",
			"ec2.test.CreateVpc",
			daemon.handleEC2CreateVpc,
			&ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")},
		},
		{
			"DeleteVpc",
			"ec2.test.DeleteVpc",
			daemon.handleEC2DeleteVpc,
			&ec2.DeleteVpcInput{VpcId: aws.String("vpc-nonexistent")},
		},
		{
			"DescribeVpcs",
			"ec2.test.DescribeVpcs",
			daemon.handleEC2DescribeVpcs,
			&ec2.DescribeVpcsInput{},
		},
		{
			"CreateSubnet",
			"ec2.test.CreateSubnet",
			daemon.handleEC2CreateSubnet,
			&ec2.CreateSubnetInput{
				VpcId:     aws.String("vpc-nonexistent"),
				CidrBlock: aws.String("10.0.1.0/24"),
			},
		},
		{
			"DeleteSubnet",
			"ec2.test.DeleteSubnet",
			daemon.handleEC2DeleteSubnet,
			&ec2.DeleteSubnetInput{SubnetId: aws.String("subnet-nonexistent")},
		},
		{
			"DescribeSubnets",
			"ec2.test.DescribeSubnets",
			daemon.handleEC2DescribeSubnets,
			&ec2.DescribeSubnetsInput{},
		},
		{
			"CreateNetworkInterface",
			"ec2.test.CreateNetworkInterface",
			daemon.handleEC2CreateNetworkInterface,
			&ec2.CreateNetworkInterfaceInput{SubnetId: aws.String("subnet-nonexistent")},
		},
		{
			"DeleteNetworkInterface",
			"ec2.test.DeleteNetworkInterface",
			daemon.handleEC2DeleteNetworkInterface,
			&ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String("eni-nonexistent")},
		},
		{
			"DescribeNetworkInterfaces",
			"ec2.test.DescribeNetworkInterfaces",
			daemon.handleEC2DescribeNetworkInterfaces,
			&ec2.DescribeNetworkInterfacesInput{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, err := daemon.natsConn.QueueSubscribe(tt.topic, "spinifex-workers", tt.handler)
			require.NoError(t, err)
			defer sub.Unsubscribe()

			reqData, err := json.Marshal(tt.input)
			require.NoError(t, err)

			reply, err := natsRequest(daemon.natsConn, tt.topic, reqData, 5*time.Second)
			require.NoError(t, err)
			require.NotNil(t, reply)

			var resp json.RawMessage
			err = json.Unmarshal(reply.Data, &resp)
			require.NoError(t, err, "VPC response should be valid JSON: %s", string(reply.Data))
		})
	}
}

func TestDelegateHandlers_IGW(t *testing.T) {
	daemon := createVPCTestDaemon(t)

	tests := []struct {
		name    string
		topic   string
		handler func(*nats.Msg)
		input   any
	}{
		{
			"CreateInternetGateway",
			"ec2.test.CreateInternetGateway",
			daemon.handleEC2CreateInternetGateway,
			&ec2.CreateInternetGatewayInput{},
		},
		{
			"DeleteInternetGateway",
			"ec2.test.DeleteInternetGateway",
			daemon.handleEC2DeleteInternetGateway,
			&ec2.DeleteInternetGatewayInput{InternetGatewayId: aws.String("igw-nonexistent")},
		},
		{
			"DescribeInternetGateways",
			"ec2.test.DescribeInternetGateways",
			daemon.handleEC2DescribeInternetGateways,
			&ec2.DescribeInternetGatewaysInput{},
		},
		{
			"AttachInternetGateway",
			"ec2.test.AttachInternetGateway",
			daemon.handleEC2AttachInternetGateway,
			&ec2.AttachInternetGatewayInput{
				InternetGatewayId: aws.String("igw-nonexistent"),
				VpcId:             aws.String("vpc-nonexistent"),
			},
		},
		{
			"DetachInternetGateway",
			"ec2.test.DetachInternetGateway",
			daemon.handleEC2DetachInternetGateway,
			&ec2.DetachInternetGatewayInput{
				InternetGatewayId: aws.String("igw-nonexistent"),
				VpcId:             aws.String("vpc-nonexistent"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, err := daemon.natsConn.QueueSubscribe(tt.topic, "spinifex-workers", tt.handler)
			require.NoError(t, err)
			defer sub.Unsubscribe()

			reqData, err := json.Marshal(tt.input)
			require.NoError(t, err)

			reply, err := natsRequest(daemon.natsConn, tt.topic, reqData, 5*time.Second)
			require.NoError(t, err)
			require.NotNil(t, reply)

			var resp json.RawMessage
			err = json.Unmarshal(reply.Data, &resp)
			require.NoError(t, err, "IGW response should be valid JSON: %s", string(reply.Data))
		})
	}
}

func TestHandleEC2CreateVpc_SuccessPath(t *testing.T) {
	daemon := createVPCTestDaemon(t)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.CreateVpc", "spinifex-workers", daemon.handleEC2CreateVpc)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateVpcInput{CidrBlock: aws.String("10.100.0.0/16")}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateVpc", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.CreateVpcOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	require.NotNil(t, output.Vpc)
	assert.NotEmpty(t, *output.Vpc.VpcId)
	assert.Equal(t, "10.100.0.0/16", *output.Vpc.CidrBlock)
	assert.Equal(t, "available", *output.Vpc.State)
}

func TestHandleEC2CreateAndDescribeVpc_RoundTrip(t *testing.T) {
	daemon := createVPCTestDaemon(t)

	createSub, err := daemon.natsConn.QueueSubscribe("ec2.CreateVpc", "spinifex-workers", daemon.handleEC2CreateVpc)
	require.NoError(t, err)
	defer createSub.Unsubscribe()

	describeSub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeVpcs", "spinifex-workers", daemon.handleEC2DescribeVpcs)
	require.NoError(t, err)
	defer describeSub.Unsubscribe()

	// Create a VPC
	createInput := &ec2.CreateVpcInput{CidrBlock: aws.String("10.200.0.0/16")}
	reqData, _ := json.Marshal(createInput)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateVpc", reqData, 5*time.Second)
	require.NoError(t, err)

	var createOutput ec2.CreateVpcOutput
	require.NoError(t, json.Unmarshal(reply.Data, &createOutput))
	vpcID := *createOutput.Vpc.VpcId

	// Describe VPCs and verify the created VPC appears
	describeInput := &ec2.DescribeVpcsInput{}
	reqData, _ = json.Marshal(describeInput)
	reply, err = natsRequest(daemon.natsConn, "ec2.DescribeVpcs", reqData, 5*time.Second)
	require.NoError(t, err)

	var describeOutput ec2.DescribeVpcsOutput
	require.NoError(t, json.Unmarshal(reply.Data, &describeOutput))

	found := false
	for _, vpc := range describeOutput.Vpcs {
		if *vpc.VpcId == vpcID {
			found = true
			assert.Equal(t, "10.200.0.0/16", *vpc.CidrBlock)
		}
	}
	assert.True(t, found, "created VPC should appear in DescribeVpcs")
}

func TestHandleEC2CreateInternetGateway_SuccessPath(t *testing.T) {
	daemon := createVPCTestDaemon(t)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.CreateInternetGateway", "spinifex-workers", daemon.handleEC2CreateInternetGateway)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateInternetGatewayInput{}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateInternetGateway", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.CreateInternetGatewayOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	require.NotNil(t, output.InternetGateway)
	assert.NotEmpty(t, *output.InternetGateway.InternetGatewayId)
	assert.True(t, len(*output.InternetGateway.InternetGatewayId) > 4)
}

func TestHandleEC2CreateSubnet_SuccessPath(t *testing.T) {
	daemon := createVPCTestDaemon(t)

	createVpcSub, err := daemon.natsConn.QueueSubscribe("ec2.CreateVpc", "spinifex-workers", daemon.handleEC2CreateVpc)
	require.NoError(t, err)
	defer createVpcSub.Unsubscribe()

	createSubnetSub, err := daemon.natsConn.QueueSubscribe("ec2.CreateSubnet", "spinifex-workers", daemon.handleEC2CreateSubnet)
	require.NoError(t, err)
	defer createSubnetSub.Unsubscribe()

	// Create a VPC first
	vpcInput := &ec2.CreateVpcInput{CidrBlock: aws.String("10.50.0.0/16")}
	reqData, _ := json.Marshal(vpcInput)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateVpc", reqData, 5*time.Second)
	require.NoError(t, err)

	var vpcOutput ec2.CreateVpcOutput
	require.NoError(t, json.Unmarshal(reply.Data, &vpcOutput))
	vpcID := *vpcOutput.Vpc.VpcId

	// Create a subnet in the VPC
	subnetInput := &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.50.1.0/24"),
	}
	reqData, _ = json.Marshal(subnetInput)
	reply, err = natsRequest(daemon.natsConn, "ec2.CreateSubnet", reqData, 5*time.Second)
	require.NoError(t, err)

	var subnetOutput ec2.CreateSubnetOutput
	require.NoError(t, json.Unmarshal(reply.Data, &subnetOutput))
	require.NotNil(t, subnetOutput.Subnet)
	assert.NotEmpty(t, *subnetOutput.Subnet.SubnetId)
	assert.Equal(t, vpcID, *subnetOutput.Subnet.VpcId)
	assert.Equal(t, "10.50.1.0/24", *subnetOutput.Subnet.CidrBlock)
}

// TestDelegateHandlers_EIGW tests Egress-Only Internet Gateway delegate handlers.
// These need a JetStream-backed EIGW service since the service uses KV storage.
func TestDelegateHandlers_EIGW(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	// Create an isolated JetStream NATS server for the EIGW service
	ns, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	eigwSvc, err := handlers_ec2_eigw.NewEgressOnlyIGWServiceImplWithNATS(daemon.config, nc)
	require.NoError(t, err)
	daemon.eigwService = eigwSvc

	tests := []struct {
		name    string
		topic   string
		handler func(*nats.Msg)
		input   any
	}{
		{
			"CreateEgressOnlyInternetGateway",
			"ec2.test.CreateEgressOnlyIGW",
			daemon.handleEC2CreateEgressOnlyInternetGateway,
			&ec2.CreateEgressOnlyInternetGatewayInput{VpcId: aws.String("vpc-123")},
		},
		{
			"DeleteEgressOnlyInternetGateway",
			"ec2.test.DeleteEgressOnlyIGW",
			daemon.handleEC2DeleteEgressOnlyInternetGateway,
			&ec2.DeleteEgressOnlyInternetGatewayInput{EgressOnlyInternetGatewayId: aws.String("eigw-nonexistent")},
		},
		{
			"DescribeEgressOnlyInternetGateways",
			"ec2.test.DescribeEgressOnlyIGWs",
			daemon.handleEC2DescribeEgressOnlyInternetGateways,
			&ec2.DescribeEgressOnlyInternetGatewaysInput{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, err := daemon.natsConn.QueueSubscribe(tt.topic, "spinifex-workers", tt.handler)
			require.NoError(t, err)
			defer sub.Unsubscribe()

			reqData, err := json.Marshal(tt.input)
			require.NoError(t, err)

			reply, err := natsRequest(daemon.natsConn, tt.topic, reqData, 5*time.Second)
			require.NoError(t, err)
			require.NotNil(t, reply)

			var resp json.RawMessage
			err = json.Unmarshal(reply.Data, &resp)
			require.NoError(t, err, "response should be valid JSON: %s", string(reply.Data))
		})
	}
}

// --- handleEC2ModifyVolume success path ---

func TestHandleEC2ModifyVolume_Success(t *testing.T) {
	daemon, store := createFullTestDaemonWithStore(t, sharedNATSURL)

	// Seed a volume in the store
	volumeID := "vol-modify-success"
	wrapper := struct {
		VolumeConfig viperblock.VolumeConfig `json:"VolumeConfig"`
	}{
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				VolumeID:   volumeID,
				SizeGiB:    10,
				State:      "available",
				VolumeType: "gp3",
			},
		},
	}
	data, _ := json.Marshal(wrapper)
	store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})

	// Subscribe a dummy ebs.sync handler so the NATS Request doesn't time out
	syncSub, err := daemon.natsConn.Subscribe("ebs.sync", func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{}`))
	})
	require.NoError(t, err)
	defer syncSub.Unsubscribe()

	sub, err := daemon.natsConn.QueueSubscribe("ec2.ModifyVolume", "spinifex-workers", daemon.handleEC2ModifyVolume)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.ModifyVolumeInput{
		VolumeId: aws.String(volumeID),
		Size:     aws.Int64(20),
	}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.ModifyVolume", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.ModifyVolumeOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	require.NotNil(t, output.VolumeModification)
	assert.Equal(t, volumeID, *output.VolumeModification.VolumeId)
	assert.Equal(t, int64(10), *output.VolumeModification.OriginalSize)
	assert.Equal(t, int64(20), *output.VolumeModification.TargetSize)
	assert.Equal(t, "completed", *output.VolumeModification.ModificationState)
}

// --- handleEC2TerminateStoppedInstance with volumes ---

func TestHandleEC2TerminateStoppedInstance_WithVolumes(t *testing.T) {
	daemon := createFullTestDaemonWithJetStream(t, sharedJSNATSURL)

	// Subscribe a dummy ebs.delete handler
	ebsDeleteSub, err := daemon.natsConn.Subscribe("ebs.delete", func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{"status":"deleted"}`))
	})
	require.NoError(t, err)
	defer ebsDeleteSub.Unsubscribe()

	stoppedVM := &vm.VM{
		ID:           "i-term-vol-001",
		Status:       vm.StateStopped,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		LastNode:     "node-1",
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-term-vol-001"),
			OwnerId:       aws.String("123456789012"),
		},
		Instance: &ec2.Instance{
			InstanceId:   aws.String("i-term-vol-001"),
			InstanceType: aws.String(getTestInstanceType(t)),
		},
	}
	// Add EFI, CloudInit, and a user volume with DeleteOnTermination
	stoppedVM.EBSRequests.Requests = []types.EBSRequest{
		{Name: "vol-efi-001", EFI: true},
		{Name: "vol-ci-001", CloudInit: true},
		{Name: "vol-user-001", DeleteOnTermination: true},
		{Name: "vol-keep-001", DeleteOnTermination: false},
	}

	err = daemon.jsManager.WriteStoppedInstance(stoppedVM.ID, stoppedVM)
	require.NoError(t, err)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.terminate", "spinifex-workers", daemon.handleEC2TerminateStoppedInstance)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reqData, _ := json.Marshal(map[string]string{"instance_id": "i-term-vol-001"})
	reply, err := natsRequest(daemon.natsConn, "ec2.terminate", reqData, 10*time.Second)
	require.NoError(t, err)

	var resp map[string]string
	err = json.Unmarshal(reply.Data, &resp)
	require.NoError(t, err)
	assert.Equal(t, "terminated", resp["status"])
	assert.Equal(t, "i-term-vol-001", resp["instanceId"])

	loaded, err := daemon.jsManager.LoadStoppedInstance("i-term-vol-001")
	require.NoError(t, err)
	assert.Nil(t, loaded)
}

// --- handleEC2DescribeInstanceTypes with capacity filter ---

func TestHandleEC2DescribeInstanceTypes_CapacityFilter(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeInstanceTypes", "spinifex-workers", daemon.handleEC2DescribeInstanceTypes)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Request with capacity=true filter
	input := &ec2.DescribeInstanceTypesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("capacity"),
				Values: []*string{aws.String("true")},
			},
		},
	}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.DescribeInstanceTypes", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstanceTypesOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	// Should return instance types that fit the node's available capacity
	assert.NotNil(t, output.InstanceTypes)
}

func TestHandleEC2DescribeInstanceTypes_NoFilter(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeInstanceTypes.nofilter", "spinifex-workers", daemon.handleEC2DescribeInstanceTypes)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.DescribeInstanceTypesInput{}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.DescribeInstanceTypes.nofilter", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstanceTypesOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.NotNil(t, output.InstanceTypes)
	assert.Greater(t, len(output.InstanceTypes), 0)
}

// --- handleEC2StartStoppedInstance: instance type not available ---

func TestHandleEC2StartStoppedInstance_InstanceTypeNotAvailable(t *testing.T) {
	daemon := createFullTestDaemonWithJetStream(t, sharedJSNATSURL)

	stoppedVM := &vm.VM{
		ID:           "i-start-badtype-001",
		Status:       vm.StateStopped,
		InstanceType: "z99.nonexistent",
		AccountID:    testAccountID,
		Instance: &ec2.Instance{
			InstanceId:   aws.String("i-start-badtype-001"),
			InstanceType: aws.String("z99.nonexistent"),
		},
	}
	err := daemon.jsManager.WriteStoppedInstance(stoppedVM.ID, stoppedVM)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(stoppedVM.ID) })

	sub, err := daemon.natsConn.QueueSubscribe("ec2.start", "spinifex-workers", daemon.handleEC2StartStoppedInstance)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reqData, _ := json.Marshal(map[string]string{"instance_id": "i-start-badtype-001"})
	reply, err := natsRequest(daemon.natsConn, "ec2.start", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

// --- handleEC2CreateImage: running instance with valid root volume ---

func TestHandleEC2CreateImage_RunningInstanceReachesService(t *testing.T) {
	daemon, store := createFullTestDaemonWithStore(t, sharedNATSURL)

	instanceID := "i-create-image-running"
	rootVolumeID := "vol-root-img-001"
	sourceImageID := "ami-source-001"

	// Seed a root volume config
	wrapper := struct {
		VolumeConfig viperblock.VolumeConfig `json:"VolumeConfig"`
	}{
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				VolumeID: rootVolumeID,
				SizeGiB:  8,
				State:    "in-use",
			},
		},
	}
	volData, _ := json.Marshal(wrapper)
	store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(rootVolumeID + "/config.json"),
		Body:   strings.NewReader(string(volData)),
	})

	daemon.Instances.Mu.Lock()
	daemon.Instances.VMS[instanceID] = &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		InstanceType: getTestInstanceType(t),
		AccountID:    testAccountID,
		Instance: &ec2.Instance{
			InstanceId: aws.String(instanceID),
			ImageId:    aws.String(sourceImageID),
			BlockDeviceMappings: []*ec2.InstanceBlockDeviceMapping{
				{
					DeviceName: aws.String("/dev/sda1"),
					Ebs: &ec2.EbsInstanceBlockDevice{
						VolumeId: aws.String(rootVolumeID),
					},
				},
			},
		},
	}
	daemon.Instances.Mu.Unlock()

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.%s.CreateImage", instanceID),
		daemon.handleEC2CreateImage,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateImageInput{
		InstanceId: aws.String(instanceID),
		Name:       aws.String("test-image-snapshot"),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.%s.CreateImage", instanceID),
		reqData,
		5*time.Second,
	)
	require.NoError(t, err)
	// The service call may fail (no real S3 backend), but we've exercised
	// the handler path up through service invocation: instance lookup,
	// state validation, root volume extraction, params construction.
	assert.NotEmpty(t, reply.Data)
}

// --- handleAttachVolume: missing volume data ---

func TestAttachVolume_MissingVolumeData(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	instanceID := "i-attach-missing-vol"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	daemon.Instances.VMS[instanceID] = instance

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// AttachVolume with nil AttachVolumeData
	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			AttachVolume: true,
		},
		AttachVolumeData: nil,
	}
	cmdData, _ := json.Marshal(command)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "InvalidParameterValue")
}

func TestAttachVolume_InstanceNotRunning(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	instanceID := "i-attach-stopped"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateStopped,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	daemon.Instances.VMS[instanceID] = instance

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			AttachVolume: true,
		},
		AttachVolumeData: &types.AttachVolumeData{
			VolumeID: "vol-test-123",
		},
	}
	cmdData, _ := json.Marshal(command)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "IncorrectInstanceState")
}

func TestAttachVolume_VolumeNotFound(t *testing.T) {
	daemon, _ := createFullTestDaemonWithStore(t, sharedNATSURL)

	instanceID := "i-attach-vol-notfound"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	daemon.Instances.VMS[instanceID] = instance

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			AttachVolume: true,
		},
		AttachVolumeData: &types.AttachVolumeData{
			VolumeID: "vol-nonexistent-999",
		},
	}
	cmdData, _ := json.Marshal(command)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "InvalidVolume.NotFound")
}

func TestAttachVolume_VolumeInUse(t *testing.T) {
	daemon, store := createFullTestDaemonWithStore(t, sharedNATSURL)

	instanceID := "i-attach-vol-inuse"
	volumeID := "vol-in-use-001"

	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	daemon.Instances.VMS[instanceID] = instance

	// Seed a volume that is already in-use
	wrapper := struct {
		VolumeConfig viperblock.VolumeConfig `json:"VolumeConfig"`
	}{
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				VolumeID: volumeID,
				SizeGiB:  10,
				State:    "in-use",
			},
		},
	}
	data, _ := json.Marshal(wrapper)
	store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			AttachVolume: true,
		},
		AttachVolumeData: &types.AttachVolumeData{
			VolumeID: volumeID,
		},
	}
	cmdData, _ := json.Marshal(command)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "VolumeInUse")
}

// --- handleEC2Events: detach volume validation ---

func TestDetachVolume_MissingVolumeData(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	instanceID := "i-detach-missing"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	daemon.Instances.VMS[instanceID] = instance

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			DetachVolume: true,
		},
		DetachVolumeData: nil,
	}
	cmdData, _ := json.Marshal(command)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "InvalidParameterValue")
}

func TestDetachVolume_InstanceNotRunning(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	instanceID := "i-detach-stopped"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateStopped,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	daemon.Instances.VMS[instanceID] = instance

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			DetachVolume: true,
		},
		DetachVolumeData: &types.DetachVolumeData{
			VolumeID: "vol-test-123",
		},
	}
	cmdData, _ := json.Marshal(command)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "IncorrectInstanceState")
}

func TestDetachVolume_VolumeNotAttached(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	instanceID := "i-detach-notattached"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	daemon.Instances.VMS[instanceID] = instance

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			DetachVolume: true,
		},
		DetachVolumeData: &types.DetachVolumeData{
			VolumeID: "vol-not-attached-999",
		},
	}
	cmdData, _ := json.Marshal(command)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "IncorrectState")
}

func TestDetachVolume_BootVolumeRejected(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	instanceID := "i-detach-boot"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	instance.EBSRequests.Requests = []types.EBSRequest{
		{Name: "vol-boot-001", Boot: true, DeviceName: "/dev/sda1"},
	}
	daemon.Instances.VMS[instanceID] = instance

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			DetachVolume: true,
		},
		DetachVolumeData: &types.DetachVolumeData{
			VolumeID: "vol-boot-001",
		},
	}
	cmdData, _ := json.Marshal(command)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "OperationNotPermitted")
}

func TestDetachVolume_DeviceMismatch(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	instanceID := "i-detach-devmismatch"
	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	instance.EBSRequests.Requests = []types.EBSRequest{
		{Name: "vol-mismatch-001", DeviceName: "/dev/sdf"},
	}
	daemon.Instances.VMS[instanceID] = instance

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			DetachVolume: true,
		},
		DetachVolumeData: &types.DetachVolumeData{
			VolumeID: "vol-mismatch-001",
			Device:   "/dev/sdg",
		},
	}
	cmdData, _ := json.Marshal(command)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "InvalidParameterValue")
}

// --- handleEC2RunInstances: insufficient capacity ---

func TestHandleEC2RunInstances_InsufficientCapacity(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.RunInstances", "spinifex-workers", daemon.handleEC2RunInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Request a very large instance count that can't be satisfied
	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String(getTestInstanceType(t)),
		MinCount:     aws.Int64(9999),
		MaxCount:     aws.Int64(9999),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.RunInstances", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2RunInstances_UnsupportedInstanceType(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.RunInstances.badtype", "spinifex-workers", daemon.handleEC2RunInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("z99.nonexistent"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.RunInstances.badtype", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

func TestHandleEC2RunInstances_MalformedInput(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.RunInstances.bad", "spinifex-workers", daemon.handleEC2RunInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request("ec2.RunInstances.bad", []byte(`{not valid}`), 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	err = json.Unmarshal(reply.Data, &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp, "Code")
}

// --- handleEC2DescribeInstances: malformed instance ID ---

func TestHandleEC2DescribeInstances_MalformedInstanceID(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	sub, err := daemon.natsConn.Subscribe("ec2.DescribeInstances.malformed", daemon.handleEC2DescribeInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String("not-an-instance-id")},
	}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request("ec2.DescribeInstances.malformed", reqData, 5*time.Second)
	require.NoError(t, err)

	assert.Contains(t, string(reply.Data), "InvalidInstanceID.Malformed")
}

// --- Terminated instance handler tests ---

func TestHandleEC2DescribeTerminatedInstances_ReturnsTerminatedInstances(t *testing.T) {
	daemon := createFullTestDaemonWithJetStream(t, sharedJSNATSURL)

	terminatedVM := &vm.VM{
		ID:           "i-describe-term-001",
		Status:       vm.StateTerminated,
		InstanceType: getTestInstanceType(t),
		LastNode:     "node-1",
		AccountID:    testAccountID,
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-term-001"),
			OwnerId:       aws.String("123456789012"),
		},
		Instance: &ec2.Instance{
			InstanceId:   aws.String("i-describe-term-001"),
			InstanceType: aws.String(getTestInstanceType(t)),
		},
	}
	err := daemon.jsManager.WriteTerminatedInstance(terminatedVM.ID, terminatedVM)
	require.NoError(t, err)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeTerminatedInstances", "spinifex-workers", daemon.handleEC2DescribeTerminatedInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.DescribeInstancesInput{}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeTerminatedInstances", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstancesOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)

	found := false
	for _, res := range output.Reservations {
		for _, inst := range res.Instances {
			if inst.InstanceId != nil && *inst.InstanceId == "i-describe-term-001" {
				found = true
				assert.Equal(t, "terminated", *inst.State.Name)
				assert.Equal(t, int64(48), *inst.State.Code)
			}
		}
	}
	assert.True(t, found, "Should find terminated instance in DescribeTerminatedInstances output")

	_ = daemon.jsManager.DeleteTerminatedInstance(terminatedVM.ID)
}

func TestHandleEC2DescribeTerminatedInstances_WithFilter(t *testing.T) {
	daemon := createFullTestDaemonWithJetStream(t, sharedJSNATSURL)

	for _, id := range []string{"i-tfilter-001", "i-tfilter-002"} {
		v := &vm.VM{
			ID:        id,
			Status:    vm.StateTerminated,
			LastNode:  "node-1",
			AccountID: testAccountID,
			Reservation: &ec2.Reservation{
				ReservationId: aws.String("r-tfilter"),
				OwnerId:       aws.String("123456789012"),
			},
			Instance: &ec2.Instance{
				InstanceId:   aws.String(id),
				InstanceType: aws.String(getTestInstanceType(t)),
			},
		}
		require.NoError(t, daemon.jsManager.WriteTerminatedInstance(v.ID, v))
	}

	sub, err := daemon.natsConn.QueueSubscribe("ec2.DescribeTerminatedInstances", "spinifex-workers", daemon.handleEC2DescribeTerminatedInstances)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Filter for only one instance
	input := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String("i-tfilter-001")},
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeTerminatedInstances", reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.DescribeInstancesOutput
	require.NoError(t, json.Unmarshal(reply.Data, &output))

	instanceCount := 0
	for _, res := range output.Reservations {
		instanceCount += len(res.Instances)
	}
	assert.Equal(t, 1, instanceCount, "Filter should return only the requested instance")

	_ = daemon.jsManager.DeleteTerminatedInstance("i-tfilter-001")
	_ = daemon.jsManager.DeleteTerminatedInstance("i-tfilter-002")
}

func TestHandleEC2TerminateStoppedInstance_WritesToTerminatedKV(t *testing.T) {
	daemon := createFullTestDaemonWithJetStream(t, sharedJSNATSURL)

	stoppedVM := &vm.VM{
		ID:           "i-stop-to-term-001",
		Status:       vm.StateStopped,
		InstanceType: getTestInstanceType(t),
		AccountID:    testAccountID,
	}
	require.NoError(t, daemon.jsManager.WriteStoppedInstance(stoppedVM.ID, stoppedVM))

	sub, err := daemon.natsConn.QueueSubscribe("ec2.terminate", "spinifex-workers", daemon.handleEC2TerminateStoppedInstance)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reqData, _ := json.Marshal(terminateStoppedInstanceRequest{InstanceID: stoppedVM.ID})
	reply, err := natsRequest(daemon.natsConn, "ec2.terminate", reqData, 30*time.Second)
	require.NoError(t, err)
	assert.Contains(t, string(reply.Data), "terminated")

	// Verify instance is in terminated KV
	terminatedInst, err := daemon.jsManager.LoadTerminatedInstance(stoppedVM.ID)
	require.NoError(t, err)
	require.NotNil(t, terminatedInst, "terminated instance should exist in terminated KV")
	assert.Equal(t, vm.StateTerminated, terminatedInst.Status)

	// Verify instance is removed from stopped KV
	stoppedInst, err := daemon.jsManager.LoadStoppedInstance(stoppedVM.ID)
	require.NoError(t, err)
	assert.Nil(t, stoppedInst, "instance should be removed from stopped KV")

	_ = daemon.jsManager.DeleteTerminatedInstance(stoppedVM.ID)
}

// --- Bead 5: EIP daemon handler tests ---

func TestDelegateHandlers_EIP(t *testing.T) {
	daemon := createVPCTestDaemon(t)

	ns, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	js, err := nc.JetStream()
	require.NoError(t, err)
	ipam, err := handlers_ec2_vpc.NewExternalIPAM(nc, js, []handlers_ec2_vpc.ExternalPoolConfig{
		{Name: "test-pool", RangeStart: "192.168.100.2", RangeEnd: "192.168.100.254", Gateway: "192.168.100.1", PrefixLen: 24},
	})
	require.NoError(t, err)

	eipSvc, err := handlers_ec2_eip.NewEIPServiceImpl(nc, ipam, daemon.vpcService)
	require.NoError(t, err)
	daemon.eipService = eipSvc

	tests := []struct {
		name    string
		topic   string
		handler func(*nats.Msg)
		input   any
	}{
		{
			"AllocateAddress",
			"ec2.test.AllocateAddress",
			daemon.handleEC2AllocateAddress,
			&ec2.AllocateAddressInput{},
		},
		{
			"ReleaseAddress",
			"ec2.test.ReleaseAddress",
			daemon.handleEC2ReleaseAddress,
			&ec2.ReleaseAddressInput{AllocationId: aws.String("eipalloc-nonexistent")},
		},
		{
			"AssociateAddress",
			"ec2.test.AssociateAddress",
			daemon.handleEC2AssociateAddress,
			&ec2.AssociateAddressInput{AllocationId: aws.String("eipalloc-nonexistent")},
		},
		{
			"DisassociateAddress",
			"ec2.test.DisassociateAddress",
			daemon.handleEC2DisassociateAddress,
			&ec2.DisassociateAddressInput{AssociationId: aws.String("eipassoc-nonexistent")},
		},
		{
			"DescribeAddresses",
			"ec2.test.DescribeAddresses",
			daemon.handleEC2DescribeAddresses,
			&ec2.DescribeAddressesInput{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, err := daemon.natsConn.QueueSubscribe(tt.topic, "spinifex-workers", tt.handler)
			require.NoError(t, err)
			defer sub.Unsubscribe()

			reqData, err := json.Marshal(tt.input)
			require.NoError(t, err)

			reply, err := natsRequest(daemon.natsConn, tt.topic, reqData, 5*time.Second)
			require.NoError(t, err)
			require.NotNil(t, reply)

			var resp json.RawMessage
			err = json.Unmarshal(reply.Data, &resp)
			require.NoError(t, err, "EIP response should be valid JSON: %s", string(reply.Data))
		})
	}
}

// --- Bead 6: Security Group daemon handler tests ---

func TestDelegateHandlers_SecurityGroup(t *testing.T) {
	daemon := createVPCTestDaemon(t)

	// Create a VPC first so SG operations have a valid target
	createVpcSub, err := daemon.natsConn.QueueSubscribe("ec2.CreateVpc", "spinifex-workers", daemon.handleEC2CreateVpc)
	require.NoError(t, err)
	defer createVpcSub.Unsubscribe()

	vpcInput := &ec2.CreateVpcInput{CidrBlock: aws.String("10.50.0.0/16")}
	reqData, _ := json.Marshal(vpcInput)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateVpc", reqData, 5*time.Second)
	require.NoError(t, err)

	var vpcOut ec2.CreateVpcOutput
	require.NoError(t, json.Unmarshal(reply.Data, &vpcOut))
	vpcID := *vpcOut.Vpc.VpcId

	tests := []struct {
		name    string
		topic   string
		handler func(*nats.Msg)
		input   any
	}{
		{
			"CreateSecurityGroup",
			"ec2.test.CreateSecurityGroup",
			daemon.handleEC2CreateSecurityGroup,
			&ec2.CreateSecurityGroupInput{
				GroupName:   aws.String("test-sg"),
				Description: aws.String("test security group"),
				VpcId:       aws.String(vpcID),
			},
		},
		{
			"DescribeSecurityGroups",
			"ec2.test.DescribeSecurityGroups",
			daemon.handleEC2DescribeSecurityGroups,
			&ec2.DescribeSecurityGroupsInput{},
		},
		{
			"AuthorizeSecurityGroupIngress",
			"ec2.test.AuthorizeSecurityGroupIngress",
			daemon.handleEC2AuthorizeSecurityGroupIngress,
			&ec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String("sg-nonexistent")},
		},
		{
			"AuthorizeSecurityGroupEgress",
			"ec2.test.AuthorizeSecurityGroupEgress",
			daemon.handleEC2AuthorizeSecurityGroupEgress,
			&ec2.AuthorizeSecurityGroupEgressInput{GroupId: aws.String("sg-nonexistent")},
		},
		{
			"RevokeSecurityGroupIngress",
			"ec2.test.RevokeSecurityGroupIngress",
			daemon.handleEC2RevokeSecurityGroupIngress,
			&ec2.RevokeSecurityGroupIngressInput{GroupId: aws.String("sg-nonexistent")},
		},
		{
			"RevokeSecurityGroupEgress",
			"ec2.test.RevokeSecurityGroupEgress",
			daemon.handleEC2RevokeSecurityGroupEgress,
			&ec2.RevokeSecurityGroupEgressInput{GroupId: aws.String("sg-nonexistent")},
		},
		{
			"DeleteSecurityGroup",
			"ec2.test.DeleteSecurityGroup",
			daemon.handleEC2DeleteSecurityGroup,
			&ec2.DeleteSecurityGroupInput{GroupId: aws.String("sg-nonexistent")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, err := daemon.natsConn.QueueSubscribe(tt.topic, "spinifex-workers", tt.handler)
			require.NoError(t, err)
			defer sub.Unsubscribe()

			reqData, err := json.Marshal(tt.input)
			require.NoError(t, err)

			reply, err := natsRequest(daemon.natsConn, tt.topic, reqData, 5*time.Second)
			require.NoError(t, err)
			require.NotNil(t, reply)

			var resp json.RawMessage
			err = json.Unmarshal(reply.Data, &resp)
			require.NoError(t, err, "SG response should be valid JSON: %s", string(reply.Data))
		})
	}
}

// --- Bead 7: Route Table daemon handler tests ---

func TestDelegateHandlers_RouteTable(t *testing.T) {
	daemon := createVPCTestDaemon(t)

	// Route table service needs its own JetStream for KV buckets
	ns, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	rtbSvc, err := handlers_ec2_routetable.NewRouteTableServiceImplWithNATS(daemon.config, nc)
	require.NoError(t, err)
	daemon.routeTableService = rtbSvc

	tests := []struct {
		name    string
		topic   string
		handler func(*nats.Msg)
		input   any
	}{
		{
			"CreateRouteTable",
			"ec2.test.CreateRouteTable",
			daemon.handleEC2CreateRouteTable,
			&ec2.CreateRouteTableInput{VpcId: aws.String("vpc-nonexistent")},
		},
		{
			"DeleteRouteTable",
			"ec2.test.DeleteRouteTable",
			daemon.handleEC2DeleteRouteTable,
			&ec2.DeleteRouteTableInput{RouteTableId: aws.String("rtb-nonexistent")},
		},
		{
			"DescribeRouteTables",
			"ec2.test.DescribeRouteTables",
			daemon.handleEC2DescribeRouteTables,
			&ec2.DescribeRouteTablesInput{},
		},
		{
			"CreateRoute",
			"ec2.test.CreateRoute",
			daemon.handleEC2CreateRoute,
			&ec2.CreateRouteInput{RouteTableId: aws.String("rtb-nonexistent")},
		},
		{
			"DeleteRoute",
			"ec2.test.DeleteRoute",
			daemon.handleEC2DeleteRoute,
			&ec2.DeleteRouteInput{RouteTableId: aws.String("rtb-nonexistent")},
		},
		{
			"ReplaceRoute",
			"ec2.test.ReplaceRoute",
			daemon.handleEC2ReplaceRoute,
			&ec2.ReplaceRouteInput{RouteTableId: aws.String("rtb-nonexistent")},
		},
		{
			"AssociateRouteTable",
			"ec2.test.AssociateRouteTable",
			daemon.handleEC2AssociateRouteTable,
			&ec2.AssociateRouteTableInput{RouteTableId: aws.String("rtb-nonexistent")},
		},
		{
			"DisassociateRouteTable",
			"ec2.test.DisassociateRouteTable",
			daemon.handleEC2DisassociateRouteTable,
			&ec2.DisassociateRouteTableInput{AssociationId: aws.String("rtbassoc-nonexistent")},
		},
		{
			"ReplaceRouteTableAssociation",
			"ec2.test.ReplaceRouteTableAssociation",
			daemon.handleEC2ReplaceRouteTableAssociation,
			&ec2.ReplaceRouteTableAssociationInput{
				AssociationId: aws.String("rtbassoc-nonexistent"),
				RouteTableId:  aws.String("rtb-nonexistent"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, err := daemon.natsConn.QueueSubscribe(tt.topic, "spinifex-workers", tt.handler)
			require.NoError(t, err)
			defer sub.Unsubscribe()

			reqData, err := json.Marshal(tt.input)
			require.NoError(t, err)

			reply, err := natsRequest(daemon.natsConn, tt.topic, reqData, 5*time.Second)
			require.NoError(t, err)
			require.NotNil(t, reply)

			var resp json.RawMessage
			err = json.Unmarshal(reply.Data, &resp)
			require.NoError(t, err, "RouteTable response should be valid JSON: %s", string(reply.Data))
		})
	}
}

// --- Bead 8: Placement Group daemon handler tests ---

func TestDelegateHandlers_PlacementGroup(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	ns, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	pgSvc, err := handlers_ec2_placementgroup.NewPlacementGroupServiceImplWithNATS(daemon.config, nc)
	require.NoError(t, err)
	daemon.placementGroupService = pgSvc

	tests := []struct {
		name    string
		topic   string
		handler func(*nats.Msg)
		input   any
	}{
		{
			"CreatePlacementGroup",
			"ec2.test.CreatePlacementGroup",
			daemon.handleEC2CreatePlacementGroup,
			&ec2.CreatePlacementGroupInput{
				GroupName: aws.String("test-pg"),
				Strategy:  aws.String("spread"),
			},
		},
		{
			"DescribePlacementGroups",
			"ec2.test.DescribePlacementGroups",
			daemon.handleEC2DescribePlacementGroups,
			&ec2.DescribePlacementGroupsInput{},
		},
		{
			"DeletePlacementGroup",
			"ec2.test.DeletePlacementGroup",
			daemon.handleEC2DeletePlacementGroup,
			&ec2.DeletePlacementGroupInput{GroupName: aws.String("pg-nonexistent")},
		},
		{
			"ReserveSpreadNodes",
			"ec2.test.ReserveSpreadNodes",
			daemon.handleEC2ReserveSpreadNodes,
			&handlers_ec2_placementgroup.ReserveSpreadNodesInput{
				GroupName:     "pg-nonexistent",
				EligibleNodes: []string{"node-1"},
				MinCount:      1,
				MaxCount:      1,
			},
		},
		{
			"FinalizeSpreadInstances",
			"ec2.test.FinalizeSpreadInstances",
			daemon.handleEC2FinalizeSpreadInstances,
			&handlers_ec2_placementgroup.FinalizeSpreadInstancesInput{
				GroupName:     "pg-nonexistent",
				NodeInstances: map[string][]string{"node-1": {"i-123"}},
			},
		},
		{
			"ReleaseSpreadNodes",
			"ec2.test.ReleaseSpreadNodes",
			daemon.handleEC2ReleaseSpreadNodes,
			&handlers_ec2_placementgroup.ReleaseSpreadNodesInput{
				GroupName: "pg-nonexistent",
				Nodes:     []string{"node-1"},
			},
		},
		{
			"RemoveInstanceFromPlacementGroup",
			"ec2.test.RemoveInstanceFromPlacementGroup",
			daemon.handleEC2RemoveInstanceFromPlacementGroup,
			&handlers_ec2_placementgroup.RemoveInstanceInput{
				GroupName:  "pg-nonexistent",
				NodeName:   "node-1",
				InstanceID: "i-123",
			},
		},
		{
			"ReserveClusterNode",
			"ec2.test.ReserveClusterNode",
			daemon.handleEC2ReserveClusterNode,
			&handlers_ec2_placementgroup.ReserveClusterNodeInput{
				GroupName:     "pg-nonexistent",
				EligibleNodes: []string{"node-1"},
			},
		},
		{
			"FinalizeClusterInstances",
			"ec2.test.FinalizeClusterInstances",
			daemon.handleEC2FinalizeClusterInstances,
			&handlers_ec2_placementgroup.FinalizeClusterInstancesInput{
				GroupName:     "pg-nonexistent",
				NodeInstances: map[string][]string{"node-1": {"i-123"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, err := daemon.natsConn.QueueSubscribe(tt.topic, "spinifex-workers", tt.handler)
			require.NoError(t, err)
			defer sub.Unsubscribe()

			reqData, err := json.Marshal(tt.input)
			require.NoError(t, err)

			reply, err := natsRequest(daemon.natsConn, tt.topic, reqData, 5*time.Second)
			require.NoError(t, err)
			require.NotNil(t, reply)

			var resp json.RawMessage
			err = json.Unmarshal(reply.Data, &resp)
			require.NoError(t, err, "PlacementGroup response should be valid JSON: %s", string(reply.Data))
		})
	}
}

// --- Bead 9: VPC attribute daemon handler tests (untested handlers) ---

func TestDelegateHandlers_VPCAttributes(t *testing.T) {
	daemon := createVPCTestDaemon(t)

	// Create a VPC first
	createVpcSub, err := daemon.natsConn.QueueSubscribe("ec2.CreateVpc", "spinifex-workers", daemon.handleEC2CreateVpc)
	require.NoError(t, err)
	defer createVpcSub.Unsubscribe()

	vpcInput := &ec2.CreateVpcInput{CidrBlock: aws.String("10.60.0.0/16")}
	reqData, _ := json.Marshal(vpcInput)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateVpc", reqData, 5*time.Second)
	require.NoError(t, err)

	var vpcOut ec2.CreateVpcOutput
	require.NoError(t, json.Unmarshal(reply.Data, &vpcOut))
	vpcID := *vpcOut.Vpc.VpcId

	tests := []struct {
		name    string
		topic   string
		handler func(*nats.Msg)
		input   any
	}{
		{
			"ModifySubnetAttribute",
			"ec2.test.ModifySubnetAttribute",
			daemon.handleEC2ModifySubnetAttribute,
			&ec2.ModifySubnetAttributeInput{SubnetId: aws.String("subnet-nonexistent")},
		},
		{
			"ModifyVpcAttribute",
			"ec2.test.ModifyVpcAttribute",
			daemon.handleEC2ModifyVpcAttribute,
			&ec2.ModifyVpcAttributeInput{VpcId: aws.String(vpcID)},
		},
		{
			"DescribeVpcAttribute",
			"ec2.test.DescribeVpcAttribute",
			daemon.handleEC2DescribeVpcAttribute,
			&ec2.DescribeVpcAttributeInput{
				VpcId:     aws.String(vpcID),
				Attribute: aws.String("enableDnsSupport"),
			},
		},
		{
			"ModifyNetworkInterfaceAttribute",
			"ec2.test.ModifyNetworkInterfaceAttribute",
			daemon.handleEC2ModifyNetworkInterfaceAttribute,
			&ec2.ModifyNetworkInterfaceAttributeInput{NetworkInterfaceId: aws.String("eni-nonexistent")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, err := daemon.natsConn.QueueSubscribe(tt.topic, "spinifex-workers", tt.handler)
			require.NoError(t, err)
			defer sub.Unsubscribe()

			reqData, err := json.Marshal(tt.input)
			require.NoError(t, err)

			reply, err := natsRequest(daemon.natsConn, tt.topic, reqData, 5*time.Second)
			require.NoError(t, err)
			require.NotNil(t, reply)

			var resp json.RawMessage
			err = json.Unmarshal(reply.Data, &resp)
			require.NoError(t, err, "VPC attribute response should be valid JSON: %s", string(reply.Data))
		})
	}
}
