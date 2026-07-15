package handlers_ec2_instance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startTestNATSServer starts an embedded NATS server for testing.
func startTestNATSServer(t *testing.T) (*server.Server, string) {
	t.Helper()
	ns, nc := testutil.StartTestNATS(t)
	_ = nc // connection managed by testutil cleanup
	return ns, ns.ClientURL()
}

// createValidRunInstancesInput creates a valid RunInstancesInput for testing.
func createValidRunInstancesInput() *ec2.RunInstancesInput {
	return &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0abcdef1234567890"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		KeyName:      aws.String("test-key-pair"),
		SecurityGroupIds: []*string{
			aws.String("sg-0123456789abcdef0"),
		},
		SubnetId: aws.String("subnet-6e7f829e"),
		UserData: aws.String("#!/bin/bash\necho 'test'"),
	}
}

// createValidReservation creates a valid ec2.Reservation response for testing.
func createValidReservation() *ec2.Reservation {
	return &ec2.Reservation{
		ReservationId: aws.String("r-0123456789abcdef0"),
		OwnerId:       aws.String("123456789012"),
		Instances: []*ec2.Instance{
			{
				InstanceId:   aws.String("i-0123456789abcdef0"),
				InstanceType: aws.String("t3.micro"),
				ImageId:      aws.String("ami-0abcdef1234567890"),
				State: &ec2.InstanceState{
					Code: aws.Int64(0),
					Name: aws.String("pending"),
				},
				PrivateIpAddress: aws.String("10.0.1.100"),
				SubnetId:         aws.String("subnet-6e7f829e"),
			},
		},
	}
}

// TestNATSInstanceService_RunInstances_Success tests successful RunInstances operation.
func TestNATSInstanceService_RunInstances_Success(t *testing.T) {
	// Skip if LOG_IGNORE is set
	if os.Getenv("LOG_IGNORE") != "" {
		t.Setenv("LOG_IGNORE", "1")
	}

	// Start test NATS server
	ns, natsURL := startTestNATSServer(t)
	defer ns.Shutdown()

	// Create NATS connections
	nc, err := nats.Connect(natsURL)
	require.NoError(t, err, "Failed to connect to NATS")
	defer nc.Close()

	// Create mock daemon subscriber that responds with valid reservation
	// Subscribe to the per-instance-type topic (t3.micro from createValidRunInstancesInput)
	mockReservation := createValidReservation()
	_, err = nc.QueueSubscribe("ec2.RunInstances.t3.micro", "spinifex-workers", func(msg *nats.Msg) {
		// Validate that request is properly formatted
		var input ec2.RunInstancesInput
		err := json.Unmarshal(msg.Data, &input)
		if err != nil {
			t.Errorf("Mock daemon received invalid JSON: %v", err)
			return
		}

		// Send back valid reservation
		responseData, _ := json.Marshal(mockReservation)
		msg.Respond(responseData)
	})
	require.NoError(t, err, "Failed to subscribe mock daemon")

	// Create NATSInstanceService
	service := NewNATSInstanceService(nc)
	require.NotNil(t, service)

	// Create valid input
	input := createValidRunInstancesInput()

	// Call RunInstances
	reservation, err := service.RunInstances(context.Background(), input, "123456789012")

	// Verify success
	require.NoError(t, err, "RunInstances should succeed")
	require.NotNil(t, reservation, "Reservation should not be nil")

	// Verify reservation contents
	assert.Equal(t, *mockReservation.ReservationId, *reservation.ReservationId)
	assert.Equal(t, *mockReservation.OwnerId, *reservation.OwnerId)
	require.Len(t, reservation.Instances, 1)

	instance := reservation.Instances[0]
	assert.Equal(t, "i-0123456789abcdef0", *instance.InstanceId)
	assert.Equal(t, "t3.micro", *instance.InstanceType)
	assert.Equal(t, int64(0), *instance.State.Code)
	assert.Equal(t, "pending", *instance.State.Name)
}

// TestNATSInstanceService_RunInstances_DaemonError tests error handling when daemon returns error.
func TestNATSInstanceService_RunInstances_DaemonError(t *testing.T) {
	if os.Getenv("LOG_IGNORE") != "" {
		t.Setenv("LOG_IGNORE", "1")
	}

	ns, natsURL := startTestNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	// Create mock daemon that responds with error
	// Subscribe to the per-instance-type topic for the requested type
	_, err = nc.QueueSubscribe("ec2.RunInstances.invalid.type", "spinifex-workers", func(msg *nats.Msg) {
		// Send back error response
		errorResponse := utils.GenerateErrorPayload("InvalidInstanceType")
		msg.Respond(errorResponse)
	})
	require.NoError(t, err)

	service := NewNATSInstanceService(nc)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0abcdef1234567890"),
		InstanceType: aws.String("invalid.type"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}

	// Call RunInstances
	reservation, err := service.RunInstances(context.Background(), input, "123456789012")

	// Verify error handling - error should be just the AWS error code for gateway lookup
	require.Error(t, err, "Should return error when daemon returns error")
	assert.Nil(t, reservation, "Reservation should be nil on error")
	assert.Equal(t, "InvalidInstanceType", err.Error(), "Error should be the AWS error code")
}

// TestNATSInstanceService_RunInstances_NoSubscriber tests behavior when no daemon is subscribed.
func TestNATSInstanceService_RunInstances_NoSubscriber(t *testing.T) {
	if os.Getenv("LOG_IGNORE") != "" {
		t.Setenv("LOG_IGNORE", "1")
	}

	ns, natsURL := startTestNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	// No subscriber - no daemon running

	service := NewNATSInstanceService(nc)
	input := createValidRunInstancesInput()

	// Call RunInstances
	start := time.Now()
	reservation, err := service.RunInstances(context.Background(), input, "123456789012")
	duration := time.Since(start)

	// Verify error behavior
	// When no subscribers are available, NATS returns ErrNoResponders immediately
	require.Error(t, err, "Should fail when no subscriber")
	assert.Nil(t, reservation, "Reservation should be nil")
	assert.ErrorIs(t, err, nats.ErrNoResponders, "Error should be ErrNoResponders")

	// Should fail quickly (within 1 second), not wait for 30 second timeout
	assert.Less(t, duration.Seconds(), 1.0, "Should fail immediately when no responders")
}

// TestNATSInstanceService_RunInstances_InvalidResponse tests handling of malformed responses.
func TestNATSInstanceService_RunInstances_InvalidResponse(t *testing.T) {
	if os.Getenv("LOG_IGNORE") != "" {
		t.Setenv("LOG_IGNORE", "1")
	}

	ns, natsURL := startTestNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	// Create mock daemon that responds with invalid JSON
	_, err = nc.QueueSubscribe("ec2.RunInstances.t3.micro", "spinifex-workers", func(msg *nats.Msg) {
		// Send back malformed JSON
		msg.Respond([]byte(`{"invalid": json response`))
	})
	require.NoError(t, err)

	service := NewNATSInstanceService(nc)
	input := createValidRunInstancesInput()

	// Call RunInstances
	reservation, err := service.RunInstances(context.Background(), input, "123456789012")

	// Verify error handling
	require.Error(t, err, "Should return error for invalid JSON")
	assert.Nil(t, reservation, "Reservation should be nil")
	assert.Contains(t, err.Error(), "failed to unmarshal", "Error should indicate unmarshal failure")
}

// TestNATSInstanceService_RunInstances_MarshalError tests handling when input marshaling fails.
func TestNATSInstanceService_RunInstances_MarshalError(t *testing.T) {
	if os.Getenv("LOG_IGNORE") != "" {
		t.Setenv("LOG_IGNORE", "1")
	}

	ns, natsURL := startTestNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	service := NewNATSInstanceService(nc)

	// Nil input should be rejected before hitting NATS
	reservation, err := service.RunInstances(context.Background(), nil, "123456789012")
	require.Error(t, err, "Should handle nil input")
	assert.Nil(t, reservation)
	assert.Contains(t, err.Error(), "instance type is required")
}

// TestNATSInstanceService_RunInstances_MultipleInstances tests launching multiple instances.
func TestNATSInstanceService_RunInstances_MultipleInstances(t *testing.T) {
	if os.Getenv("LOG_IGNORE") != "" {
		t.Setenv("LOG_IGNORE", "1")
	}

	ns, natsURL := startTestNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	// Create mock daemon that returns multiple instances
	_, err = nc.QueueSubscribe("ec2.RunInstances.t3.micro", "spinifex-workers", func(msg *nats.Msg) {
		var input ec2.RunInstancesInput
		json.Unmarshal(msg.Data, &input)

		// Create reservation with count matching MinCount
		count := int(*input.MinCount)
		instances := make([]*ec2.Instance, count)
		for i := range count {
			instanceId := aws.String(aws.StringValue(aws.String(fmt.Sprintf("i-%d", i))))
			instances[i] = &ec2.Instance{
				InstanceId:   instanceId,
				InstanceType: input.InstanceType,
				ImageId:      input.ImageId,
				State: &ec2.InstanceState{
					Code: aws.Int64(0),
					Name: aws.String("pending"),
				},
			}
		}

		reservation := &ec2.Reservation{
			ReservationId: aws.String("r-0123456789abcdef0"),
			Instances:     instances,
		}

		responseData, _ := json.Marshal(reservation)
		msg.Respond(responseData)
	})
	require.NoError(t, err)

	service := NewNATSInstanceService(nc)

	// Request 3 instances
	input := createValidRunInstancesInput()
	input.MinCount = aws.Int64(3)
	input.MaxCount = aws.Int64(3)

	reservation, err := service.RunInstances(context.Background(), input, "123456789012")

	// Verify multiple instances returned
	require.NoError(t, err)
	require.NotNil(t, reservation)
	assert.Len(t, reservation.Instances, 3, "Should have 3 instances")
}

// TestNATSInstanceService_RunInstances_QueueGroup tests load balancing with queue groups.
func TestNATSInstanceService_RunInstances_QueueGroup(t *testing.T) {
	if os.Getenv("LOG_IGNORE") != "" {
		t.Setenv("LOG_IGNORE", "1")
	}

	ns, natsURL := startTestNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	// Create multiple mock daemon workers in same queue group
	worker1Count := 0
	worker2Count := 0

	_, err = nc.QueueSubscribe("ec2.RunInstances.t3.micro", "spinifex-workers", func(msg *nats.Msg) {
		worker1Count++
		reservation := createValidReservation()
		responseData, _ := json.Marshal(reservation)
		msg.Respond(responseData)
	})
	require.NoError(t, err)

	_, err = nc.QueueSubscribe("ec2.RunInstances.t3.micro", "spinifex-workers", func(msg *nats.Msg) {
		worker2Count++
		reservation := createValidReservation()
		responseData, _ := json.Marshal(reservation)
		msg.Respond(responseData)
	})
	require.NoError(t, err)

	service := NewNATSInstanceService(nc)
	input := createValidRunInstancesInput()

	// Send multiple requests
	requestCount := 10
	for range requestCount {
		reservation, err := service.RunInstances(context.Background(), input, "123456789012")
		require.NoError(t, err)
		require.NotNil(t, reservation)
	}

	// Verify load was distributed (both workers should have handled some requests)
	// With queue groups, NATS distributes messages across subscribers
	t.Logf("Worker 1 handled: %d requests", worker1Count)
	t.Logf("Worker 2 handled: %d requests", worker2Count)

	assert.Greater(t, worker1Count, 0, "Worker 1 should handle some requests")
	assert.Greater(t, worker2Count, 0, "Worker 2 should handle some requests")
	assert.Equal(t, requestCount, worker1Count+worker2Count, "All requests should be handled")
}
