package handlers_ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/stretchr/testify/require"
)

// TestSchedulerPasses_EnumerationFailureIsReported pins the contract every
// leader-only sweep depends on: the KV bucket lister closes its channel the same
// way whether the listing completed or failed, so a pass that ignored the
// terminal error would read an unreachable JetStream as an empty fleet and
// report a clean tick while every account went unswept — instances never
// drained, services never relaunched, stopped tasks never pruned.
func TestSchedulerPasses_EnumerationFailureIsReported(t *testing.T) {
	svc, nc := newTestService(t)
	sc := NewScheduler(nc, svc, "test-holder")
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)

	passes := map[string]func(context.Context) error{
		"reap":                 sc.reap,
		"sweepStoppedTasks":    sc.sweepStoppedTasks,
		"reconcileAllServices": svc.reconcileAllServices,
	}
	for name, pass := range passes {
		t.Run(name+"/reachable", func(t *testing.T) {
			require.NoError(t, pass(t.Context()), "a complete enumeration must complete the pass")
		})
	}

	// Closing the connection fails the stream-names request behind the listing.
	nc.Close()
	for name, pass := range passes {
		t.Run(name+"/unreachable", func(t *testing.T) {
			require.Error(t, pass(t.Context()), "a failed bucket enumeration must not report a completed pass")
		})
	}
}
