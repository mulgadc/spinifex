// Exercises the unexported RDS launcher internals with no exported
// surface to drive them through.
//
//test:in-package
package handlers_ochrevector

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRDSApplianceLauncher_LaunchCreatesAndPollsToAvailable pins Launch's
// happy path: a well-formed CreateDBInstance request, then a poll loop over
// DescribeDBInstances that keeps going while the instance is still
// "creating" and returns the endpoint once it reports "available".
func TestRDSApplianceLauncher_LaunchCreatesAndPollsToAvailable(t *testing.T) {
	orig := launchPollInterval
	launchPollInterval = 5 * time.Millisecond
	defer func() { launchPollInterval = orig }()

	_, nc := testutil.StartTestNATS(t)
	defer nc.Close()

	var mu sync.Mutex
	createCalls := 0
	var lastCreate rds.CreateDBInstanceInput

	createSub, err := nc.Subscribe(rdsCreateSubject, func(msg *nats.Msg) {
		mu.Lock()
		createCalls++
		_ = json.Unmarshal(msg.Data, &lastCreate)
		mu.Unlock()
		out, _ := json.Marshal(rds.CreateDBInstanceOutput{})
		_ = msg.Respond(out)
	})
	require.NoError(t, err)
	defer func() { _ = createSub.Unsubscribe() }()

	describeCalls := 0
	describeSub, err := nc.Subscribe(rdsDescribeSubject, func(msg *nats.Msg) {
		mu.Lock()
		describeCalls++
		call := describeCalls
		mu.Unlock()

		status := "creating"
		var endpoint *rds.Endpoint
		if call >= 2 {
			status = rdsAvailableStatus
			endpoint = &rds.Endpoint{Address: aws.String("appliance.internal"), Port: aws.Int64(5432)}
		}
		out, _ := json.Marshal(rds.DescribeDBInstancesOutput{
			DBInstances: []*rds.DBInstance{{
				DBInstanceIdentifier: aws.String("ochre-vector-pg"),
				DBInstanceStatus:     aws.String(status),
				Endpoint:             endpoint,
			}},
		})
		_ = msg.Respond(out)
	})
	require.NoError(t, err)
	defer func() { _ = describeSub.Unsubscribe() }()

	launcher := NewRDSApplianceLauncher(nc, 5*time.Second)
	endpoint, port, err := launcher.Launch(t.Context(), "ochre-vector-pg", "ochre_vector_admin", "s3cr3t-pw")
	require.NoError(t, err)
	assert.Equal(t, "appliance.internal", endpoint)
	assert.Equal(t, 5432, port)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, createCalls, "create must be requested exactly once")
	assert.GreaterOrEqual(t, describeCalls, 2, "must poll past the first non-available observation")
	assert.Equal(t, "ochre-vector-pg", aws.StringValue(lastCreate.DBInstanceIdentifier))
	assert.Equal(t, "postgres", aws.StringValue(lastCreate.Engine))
	assert.Equal(t, "ochre_vector_admin", aws.StringValue(lastCreate.MasterUsername))
	assert.Equal(t, "s3cr3t-pw", aws.StringValue(lastCreate.MasterUserPassword))
}

// TestRDSApplianceLauncher_LaunchIsIdempotentOnAlreadyExists pins Launch's
// re-entrant recovery path (Appliance.resume calling Launch a second time for
// an identifier it already launched): a DBInstanceAlreadyExists response from
// CreateDBInstance must not fail the launch, only the poll result matters.
func TestRDSApplianceLauncher_LaunchIsIdempotentOnAlreadyExists(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	defer nc.Close()

	createSub, err := nc.Subscribe(rdsCreateSubject, func(msg *nats.Msg) {
		// Mirrors handlers/rds's actual CreateDBInstance collision error
		// (awserrors.Errorf), whose Error() text carries the code as a
		// ": <code>" suffix -- the idempotency check below depends on that
		// suffix being present, so a hand-written message would not exercise
		// the real production shape.
		rdsErr := awserrors.Errorf(awserrors.ErrorDBInstanceAlreadyExists, "DB instance %s already exists", "ochre-vector-pg")
		payload := utils.GenerateErrorPayloadWithMessage(awserrors.ValidErrorCodeFromError(rdsErr), rdsErr.Error())
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	defer func() { _ = createSub.Unsubscribe() }()

	describeSub, err := nc.Subscribe(rdsDescribeSubject, func(msg *nats.Msg) {
		out, _ := json.Marshal(rds.DescribeDBInstancesOutput{
			DBInstances: []*rds.DBInstance{{
				DBInstanceIdentifier: aws.String("ochre-vector-pg"),
				DBInstanceStatus:     aws.String(rdsAvailableStatus),
				Endpoint:             &rds.Endpoint{Address: aws.String("appliance.internal"), Port: aws.Int64(5432)},
			}},
		})
		_ = msg.Respond(out)
	})
	require.NoError(t, err)
	defer func() { _ = describeSub.Unsubscribe() }()

	launcher := NewRDSApplianceLauncher(nc, 5*time.Second)
	endpoint, port, err := launcher.Launch(t.Context(), "ochre-vector-pg", "ochre_vector_admin", "s3cr3t-pw")
	require.NoError(t, err)
	assert.Equal(t, "appliance.internal", endpoint)
	assert.Equal(t, 5432, port)
}

// TestRDSApplianceLauncher_LaunchPropagatesOtherCreateErrors pins that a
// create failure unrelated to identifier collision (e.g. an invalid
// parameter) is NOT swallowed the way DBInstanceAlreadyExists is.
func TestRDSApplianceLauncher_LaunchPropagatesOtherCreateErrors(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	defer nc.Close()

	createSub, err := nc.Subscribe(rdsCreateSubject, func(msg *nats.Msg) {
		payload := utils.GenerateErrorPayloadWithMessage(awserrors.ErrorInvalidParameterValue, "DBInstanceClass is not supported")
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	defer func() { _ = createSub.Unsubscribe() }()

	launcher := NewRDSApplianceLauncher(nc, 5*time.Second)
	_, _, err = launcher.Launch(t.Context(), "ochre-vector-pg", "ochre_vector_admin", "s3cr3t-pw")
	assert.Error(t, err)
}
