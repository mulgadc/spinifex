package daemon

import (
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
