package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractLastPasswordData(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "last block wins when several are present",
			data: "<Password>Zmlyc3Q=</Password>\nnoise\n<Password>c2Vjb25k</Password>",
			want: "c2Vjb25k",
		},
		{
			name: "CRLF line endings",
			data: "boot noise\r\n<Password>Y3JsZg==</Password>\r\nmore noise\r\n",
			want: "Y3JsZg==",
		},
		{
			name: "block surrounded by unrelated console output",
			data: "Booting kernel...\nStarting services\n<Password>c3Vycm91bmRlZA==</Password>\nLogin prompt ready\n",
			want: "c3Vycm91bmRlZA==",
		},
		{
			name: "no block at all",
			data: "just regular console output\nwith no password blocks\n",
			want: "",
		},
		{
			name: "empty log content",
			data: "",
			want: "",
		},
		{
			name: "malformed unterminated opener",
			data: "<Password>dGhpc05ldmVyQ2xvc2Vz\nrest of the boot log continues here",
			want: "",
		},
		{
			name: "unterminated opener does not swallow a later valid block",
			data: "<Password>dGhpc05ldmVyQ2xvc2Vz\n<Password>dmFsaWQ=</Password>",
			want: "dmFsaWQ=",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLastPasswordData([]byte(tt.data))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHandleEC2GetPasswordData(t *testing.T) {
	natsURL := sharedNATSURL
	daemon := createFullTestDaemon(t, natsURL)

	instanceID := "i-password-test-001"

	tmpDir := t.TempDir()
	logPath := tmpDir + "/console-" + instanceID + ".log"
	logContent := "Booting...\r\n<Password>Zmlyc3Q=</Password>\r\nmore boot noise\r\n<Password>bGFzdA==</Password>\r\n"
	require.NoError(t, os.WriteFile(logPath, []byte(logContent), 0644))

	daemon.vmMgr.Insert(&vm.VM{
		ID:        instanceID,
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Config: vm.Config{
			ConsoleLogPath: logPath,
		},
	})
	topic := fmt.Sprintf("ec2.%s.GetPasswordData", instanceID)
	sub, err := daemon.natsConn.Subscribe(topic, daemon.handleEC2GetPasswordData)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetPasswordDataInput{
		InstanceId: aws.String(instanceID),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, topic, reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.GetPasswordDataOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.Equal(t, instanceID, *output.InstanceId)
	require.NotNil(t, output.PasswordData)
	assert.Equal(t, "bGFzdA==", *output.PasswordData)
	assert.NotNil(t, output.Timestamp)
}

func TestHandleEC2GetPasswordData_EmptyLog(t *testing.T) {
	natsURL := sharedNATSURL
	daemon := createFullTestDaemon(t, natsURL)

	instanceID := "i-password-empty-001"

	// Instance exists but the guest never emitted a password block.
	daemon.vmMgr.Insert(&vm.VM{
		ID:        instanceID,
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Config: vm.Config{
			ConsoleLogPath: "/nonexistent/console.log",
		},
	})
	topic := fmt.Sprintf("ec2.%s.GetPasswordData", instanceID)
	sub, err := daemon.natsConn.Subscribe(topic, daemon.handleEC2GetPasswordData)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetPasswordDataInput{
		InstanceId: aws.String(instanceID),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, topic, reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.GetPasswordDataOutput
	err = json.Unmarshal(reply.Data, &output)
	require.NoError(t, err)
	assert.Equal(t, instanceID, *output.InstanceId)
	require.NotNil(t, output.PasswordData)
	assert.Empty(t, *output.PasswordData)
}

func TestHandleEC2GetPasswordData_MissingInstanceId(t *testing.T) {
	natsURL := sharedNATSURL
	daemon := createFullTestDaemon(t, natsURL)

	topic := "ec2._.GetPasswordData"
	sub, err := daemon.natsConn.Subscribe(topic, daemon.handleEC2GetPasswordData)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetPasswordDataInput{}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request(topic, reqData, 5*time.Second)
	require.NoError(t, err)

	assert.Contains(t, string(reply.Data), awserrors.ErrorMissingParameter)
}

func TestHandleEC2GetPasswordData_MalformedInput(t *testing.T) {
	natsURL := sharedNATSURL
	daemon := createFullTestDaemon(t, natsURL)

	topic := "ec2._.GetPasswordData"
	sub, err := daemon.natsConn.Subscribe(topic, daemon.handleEC2GetPasswordData)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request(topic, []byte("not json"), 5*time.Second)
	require.NoError(t, err)

	assert.Contains(t, string(reply.Data), awserrors.ErrorValidationError)
}

func TestHandleEC2GetPasswordData_OwnershipDenied(t *testing.T) {
	natsURL := sharedNATSURL
	daemon := createFullTestDaemon(t, natsURL)

	instanceID := "i-password-other-account"

	// Instance is owned by testAccountID; the request below comes from a
	// different account and must be refused as NotFound rather than leaking
	// whether the instance exists.
	daemon.vmMgr.Insert(&vm.VM{
		ID:        instanceID,
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Config: vm.Config{
			ConsoleLogPath: "/nonexistent/console.log",
		},
	})
	topic := fmt.Sprintf("ec2.%s.GetPasswordData", instanceID)
	sub, err := daemon.natsConn.Subscribe(topic, daemon.handleEC2GetPasswordData)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetPasswordDataInput{
		InstanceId: aws.String(instanceID),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequestAs(daemon.natsConn, topic, reqData, "999999999999", 5*time.Second)
	require.NoError(t, err)

	assert.Contains(t, string(reply.Data), awserrors.ErrorInvalidInstanceIDNotFound)
}

func TestHandleEC2GetPasswordData_NotFound(t *testing.T) {
	natsURL := sharedNATSURL
	daemon := createFullTestDaemon(t, natsURL)

	instanceID := "i-nonexistent-password"
	topic := fmt.Sprintf("ec2.%s.GetPasswordData", instanceID)
	sub, err := daemon.natsConn.Subscribe(topic, daemon.handleEC2GetPasswordData)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetPasswordDataInput{
		InstanceId: aws.String(instanceID),
	}
	reqData, _ := json.Marshal(input)
	reply, err := daemon.natsConn.Request(topic, reqData, 5*time.Second)
	require.NoError(t, err)

	assert.Contains(t, string(reply.Data), "InvalidInstanceID.NotFound")
}

// importPasswordTestKey seeds a key pair on the daemon's key service so the
// launch-key type is resolvable, and returns the key name.
func importPasswordTestKey(t *testing.T, daemon *Daemon, keyName, pubKey string) string {
	t.Helper()
	_, err := daemon.keyService.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName:           aws.String(keyName),
		PublicKeyMaterial: []byte(pubKey),
	}, testAccountID)
	require.NoError(t, err)
	return keyName
}

const (
	passwordTestRSAPubKey     = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQDP9LrByKWpgbX+prxBwnlf7lrmI5AfDwuiCofuvOAzt9/PwIDMMIAhlvlpm09jjOuuH/MRQApJB5A714Auv+hBKVK0lCq9KcTrnKZOpRj2aGgIZgaoO6P/POoZc+kBf9Y/GP18DCKc4y/XyBsp69dPP6XRdYBlEdeIeVZdgCPYrM7s5FjT7aML2ba2Y2EvH116hPxv+nJZGwM6yqWxWRyTOoNMMTAfNYmoKkNy2zC1FARUyqDwumJ2z5Fvo4ZdN1qoFPOsfPc3iB0NUtSZbLU1awScvHb0rwR5PRnelTZ3Nbkw8I8A0IAhBTE5veW9D38hDIJhRd4nW73BUhgmzDL7"
	passwordTestED25519PubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
)

// An ED25519 launch key can never produce a password, so the empty result is
// replaced by an error that names the cause. Every other shape of "empty" stays
// empty, because it may still resolve on a later poll.
func TestLaunchKeyCannotEncrypt(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)

	ed := importPasswordTestKey(t, daemon, "pw-ed25519", passwordTestED25519PubKey)
	rsa := importPasswordTestKey(t, daemon, "pw-rsa", passwordTestRSAPubKey)

	windows := aws.String("windows")

	tests := []struct {
		name     string
		instance *vm.VM
		want     bool
	}{
		{
			name: "windows with an ed25519 key",
			instance: &vm.VM{AccountID: testAccountID, Instance: &ec2.Instance{
				Platform: windows, KeyName: aws.String(ed)}},
			want: true,
		},
		{
			name: "windows with an rsa key",
			instance: &vm.VM{AccountID: testAccountID, Instance: &ec2.Instance{
				Platform: windows, KeyName: aws.String(rsa)}},
			want: false,
		},
		{
			// Linux never emits a password, so it keeps the AWS empty result
			// rather than being told its key type is wrong.
			name: "linux with an ed25519 key",
			instance: &vm.VM{AccountID: testAccountID, Instance: &ec2.Instance{
				KeyName: aws.String(ed)}},
			want: false,
		},
		{
			name: "windows launched with no key pair",
			instance: &vm.VM{AccountID: testAccountID, Instance: &ec2.Instance{
				Platform: windows}},
			want: false,
		},
		{
			// A key that cannot be resolved is not evidence of the wrong type.
			name: "windows with an unresolvable key",
			instance: &vm.VM{AccountID: testAccountID, Instance: &ec2.Instance{
				Platform: windows, KeyName: aws.String("pw-missing")}},
			want: false,
		},
		{
			name:     "legacy instance with no EC2 metadata",
			instance: &vm.VM{AccountID: testAccountID},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, daemon.launchKeyCannotEncrypt(tt.instance))
		})
	}
}

// End to end: the caller sees InvalidKeyPair.Type rather than an empty string
// they would otherwise keep polling.
func TestHandleEC2GetPasswordData_NonRSALaunchKey(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)
	instanceID := "i-password-ed25519-001"

	keyName := importPasswordTestKey(t, daemon, "pw-handler-ed25519", passwordTestED25519PubKey)
	daemon.vmMgr.Insert(&vm.VM{
		ID:        instanceID,
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Config:    vm.Config{ConsoleLogPath: "/nonexistent/console.log"},
		Instance: &ec2.Instance{
			Platform: aws.String("windows"),
			KeyName:  aws.String(keyName),
		},
	})

	topic := fmt.Sprintf("ec2.%s.GetPasswordData", instanceID)
	sub, err := daemon.natsConn.Subscribe(topic, daemon.handleEC2GetPasswordData)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reqData, _ := json.Marshal(&ec2.GetPasswordDataInput{InstanceId: aws.String(instanceID)})
	reply, err := natsRequest(daemon.natsConn, topic, reqData, 5*time.Second)
	require.NoError(t, err)

	assert.Contains(t, string(reply.Data), awserrors.ErrorInvalidKeyPairType)
}

// A populated blob always wins: the key-type check only ever replaces an empty
// result, so a Windows guest that did emit one is never second-guessed.
func TestHandleEC2GetPasswordData_BlobBeatsKeyTypeCheck(t *testing.T) {
	daemon := createFullTestDaemon(t, sharedNATSURL)
	instanceID := "i-password-blob-wins-001"

	logPath := t.TempDir() + "/console.log"
	require.NoError(t, os.WriteFile(logPath, []byte("<Password>Zmlyc3Q=</Password>\n"), 0644))

	keyName := importPasswordTestKey(t, daemon, "pw-blob-ed25519", passwordTestED25519PubKey)
	daemon.vmMgr.Insert(&vm.VM{
		ID:        instanceID,
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Config:    vm.Config{ConsoleLogPath: logPath},
		Instance: &ec2.Instance{
			Platform: aws.String("windows"),
			KeyName:  aws.String(keyName),
		},
	})

	topic := fmt.Sprintf("ec2.%s.GetPasswordData", instanceID)
	sub, err := daemon.natsConn.Subscribe(topic, daemon.handleEC2GetPasswordData)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reqData, _ := json.Marshal(&ec2.GetPasswordDataInput{InstanceId: aws.String(instanceID)})
	reply, err := natsRequest(daemon.natsConn, topic, reqData, 5*time.Second)
	require.NoError(t, err)

	var output ec2.GetPasswordDataOutput
	require.NoError(t, json.Unmarshal(reply.Data, &output))
	require.NotNil(t, output.PasswordData)
	assert.Equal(t, "Zmlyc3Q=", *output.PasswordData)
}
