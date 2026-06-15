//go:build e2e

package harness

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
)

// fakeCapacityEC2 returns capErr for the first failCount RunInstances calls, then a
// non-nil reservation. Tracks how many times RunInstances was invoked.
type fakeCapacityEC2 struct {
	ec2iface.EC2API
	failCount int
	calls     int
	capErr    error
}

func (f *fakeCapacityEC2) RunInstances(*ec2.RunInstancesInput) (*ec2.Reservation, error) {
	f.calls++
	if f.calls <= f.failCount {
		return nil, f.capErr
	}
	return &ec2.Reservation{}, nil
}

func capacityErr() error {
	return awserr.New("InsufficientInstanceCapacity", "cluster full", nil)
}

func TestCapacityRetry_RetriesThenSucceeds(t *testing.T) {
	fake := &fakeCapacityEC2{failCount: 2, capErr: capacityErr()}
	c := &capacityRetryEC2{EC2API: fake, interval: time.Millisecond, maxWait: time.Minute}

	out, err := c.RunInstances(&ec2.RunInstancesInput{})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if out == nil {
		t.Fatalf("expected non-nil reservation")
	}
	if fake.calls != 3 {
		t.Fatalf("expected 3 calls (2 fail + 1 ok), got %d", fake.calls)
	}
}

func TestCapacityRetry_NonCapacityErrorReturnsImmediately(t *testing.T) {
	boom := errors.New("AuthFailure: bad creds")
	fake := &fakeCapacityEC2{failCount: 1, capErr: boom}
	c := &capacityRetryEC2{EC2API: fake, interval: time.Millisecond, maxWait: time.Minute}

	_, err := c.RunInstances(&ec2.RunInstancesInput{})
	if !errors.Is(err, boom) {
		t.Fatalf("expected the non-capacity error back, got %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("non-capacity error must not retry, got %d calls", fake.calls)
	}
}

func TestCapacityRetry_DeadlineReturnsLastError(t *testing.T) {
	// Always at capacity; a tiny maxWait forces the loop to give up and surface
	// the capacity error rather than hang.
	fake := &fakeCapacityEC2{failCount: 1 << 30, capErr: capacityErr()}
	c := &capacityRetryEC2{EC2API: fake, interval: time.Millisecond, maxWait: 5 * time.Millisecond}

	_, err := c.RunInstances(&ec2.RunInstancesInput{})
	if !isInsufficientCapacity(err) {
		t.Fatalf("expected capacity error after deadline, got %v", err)
	}
}

func TestIsInsufficientCapacity(t *testing.T) {
	if !isInsufficientCapacity(capacityErr()) {
		t.Fatalf("awserr InsufficientInstanceCapacity should match")
	}
	if !isInsufficientCapacity(errors.New("got InsufficientInstanceCapacity from gw")) {
		t.Fatalf("string-wrapped capacity error should match")
	}
	if isInsufficientCapacity(nil) {
		t.Fatalf("nil must not match")
	}
	if isInsufficientCapacity(errors.New("AuthFailure")) {
		t.Fatalf("unrelated error must not match")
	}
}
