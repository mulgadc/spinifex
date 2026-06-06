//go:build e2e

package harness

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/eks"
)

// WaitForEKSClusterActive polls DescribeCluster until status == ACTIVE. Control
// plane provisioning boots a K3s server VM + NLB, so default timeout is 10min.
func WaitForEKSClusterActive(t *testing.T, c *AWSClient, name string, opts ...PollOpt) *eks.Cluster {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 10 * time.Minute, interval: 5 * time.Second}, opts...)
	var last *eks.Cluster
	EventuallyErr(t, func() error {
		out, err := c.EKS.DescribeCluster(&eks.DescribeClusterInput{Name: aws.String(name)})
		if err != nil {
			return fmt.Errorf("describe-cluster %s: %w", name, err)
		}
		last = out.Cluster
		state := aws.StringValue(last.Status)
		switch state {
		case eks.ClusterStatusActive:
			return nil
		case eks.ClusterStatusFailed:
			return fmt.Errorf("%s entered FAILED state", name)
		default:
			return fmt.Errorf("%s status=%s want=ACTIVE", name, state)
		}
	}, cfg.timeout, cfg.interval)
	t.Logf("cluster %s reached status ACTIVE", name)
	return last
}

// WaitForEKSClusterDeleted polls DescribeCluster until it returns
// ResourceNotFoundException (cluster fully gone).
func WaitForEKSClusterDeleted(t *testing.T, c *AWSClient, name string, opts ...PollOpt) {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 10 * time.Minute, interval: 5 * time.Second}, opts...)
	EventuallyErr(t, func() error {
		_, err := c.EKS.DescribeCluster(&eks.DescribeClusterInput{Name: aws.String(name)})
		if err == nil {
			return fmt.Errorf("%s still exists", name)
		}
		var aerr awserr.Error
		if errors.As(err, &aerr) && aerr.Code() == eks.ErrCodeResourceNotFoundException {
			return nil
		}
		return fmt.Errorf("describe-cluster %s: %w", name, err)
	}, cfg.timeout, cfg.interval)
	t.Logf("cluster %s deleted", name)
}
