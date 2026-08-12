package ebsctl

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
)

func TestCollectorRecordSuccessWithSettle(t *testing.T) {
	c := newCollector()
	settle := 5 * time.Millisecond
	c.record(10*time.Millisecond, &settle, false, nil)
	c.record(20*time.Millisecond, &settle, false, nil)

	res := c.result("Op")
	if res.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", res.Attempts)
	}
	if res.APILatency.Count != 2 {
		t.Errorf("APILatency.Count = %d, want 2", res.APILatency.Count)
	}
	if res.SettleTime == nil || res.SettleTime.Count != 2 {
		t.Errorf("SettleTime = %+v, want Count=2", res.SettleTime)
	}
	if res.Errors.Total != 0 {
		t.Errorf("Errors.Total = %d, want 0", res.Errors.Total)
	}
}

func TestCollectorRecordSettleTimeout(t *testing.T) {
	c := newCollector()
	c.record(10*time.Millisecond, nil, true, nil)

	res := c.result("Op")
	if res.SettleTimeouts != 1 {
		t.Errorf("SettleTimeouts = %d, want 1", res.SettleTimeouts)
	}
	if res.SettleTime != nil {
		t.Errorf("SettleTime = %+v, want nil (no settle samples recorded)", res.SettleTime)
	}
	if res.APILatency.Count != 1 {
		t.Errorf("APILatency.Count = %d, want 1 (the API call itself still succeeded)", res.APILatency.Count)
	}
}

func TestCollectorRecordNoSettleTracked(t *testing.T) {
	c := newCollector()
	c.record(10*time.Millisecond, nil, false, nil)

	res := c.result("Op")
	if res.SettleTime != nil {
		t.Errorf("SettleTime = %+v, want nil", res.SettleTime)
	}
	if res.SettleTimeouts != 0 {
		t.Errorf("SettleTimeouts = %d, want 0", res.SettleTimeouts)
	}
}

type fakeAWSError struct{ code, msg string }

func (e fakeAWSError) Error() string   { return e.code + ": " + e.msg }
func (e fakeAWSError) Code() string    { return e.code }
func (e fakeAWSError) Message() string { return e.msg }
func (e fakeAWSError) OrigErr() error  { return nil }

var _ awserr.Error = fakeAWSError{}

func TestCollectorRecordErrorTally(t *testing.T) {
	c := newCollector()
	c.record(0, nil, false, fakeAWSError{code: "VolumeInUse", msg: "boom"})
	c.record(0, nil, false, fakeAWSError{code: "VolumeInUse", msg: "boom"})
	c.record(0, nil, false, errors.New("plain error"))

	res := c.result("Op")
	if res.Errors.Total != 3 {
		t.Errorf("Errors.Total = %d, want 3", res.Errors.Total)
	}
	if res.Errors.ByCode["VolumeInUse"] != 2 {
		t.Errorf("ByCode[VolumeInUse] = %d, want 2", res.Errors.ByCode["VolumeInUse"])
	}
	if res.Errors.ByCode["non-aws-error"] != 1 {
		t.Errorf("ByCode[non-aws-error] = %d, want 1", res.Errors.ByCode["non-aws-error"])
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
	if res.APILatency.Count != 0 {
		t.Errorf("APILatency.Count = %d, want 0 (all attempts errored)", res.APILatency.Count)
	}
	wantRate := 1.0
	if res.ErrorRate != wantRate {
		t.Errorf("ErrorRate = %v, want %v", res.ErrorRate, wantRate)
	}
}

func TestParallelForCounts(t *testing.T) {
	var mu sync.Mutex
	warmCount, measuredCount := 0, 0
	parallelFor(4, 3, 5, func(_, _ int, warm bool) {
		mu.Lock()
		defer mu.Unlock()
		if warm {
			warmCount++
		} else {
			measuredCount++
		}
	})
	if warmCount != 4*3 {
		t.Errorf("warmCount = %d, want %d", warmCount, 4*3)
	}
	if measuredCount != 4*5 {
		t.Errorf("measuredCount = %d, want %d", measuredCount, 4*5)
	}
}

func TestParallelForDefaultsConcurrencyToOne(t *testing.T) {
	var count int
	var mu sync.Mutex
	parallelFor(0, 0, 3, func(_, _ int, _ bool) {
		mu.Lock()
		count++
		mu.Unlock()
	})
	if count != 3 {
		t.Errorf("count = %d, want 3 (concurrency<1 should default to 1)", count)
	}
}

func TestDrainDeleteDistributesAllIDs(t *testing.T) {
	ids := make([]string, 10)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%d", i)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	col := drainDelete(ids, 3, 2, func(id string) (time.Duration, *time.Duration, bool, error) {
		mu.Lock()
		seen[id]++
		mu.Unlock()
		return time.Millisecond, nil, false, nil
	})
	if len(seen) != 10 {
		t.Errorf("saw %d distinct ids, want 10", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("id %s processed %d times, want 1", id, n)
		}
	}
	res := col.result("DeleteX")
	if res.Attempts != 8 {
		t.Errorf("Attempts = %d, want 8 (10 ids - 2 discarded warm-up)", res.Attempts)
	}
}

func TestDrainDeletePropagatesErrors(t *testing.T) {
	ids := []string{"a", "b", "c"}
	col := drainDelete(ids, 1, 0, func(id string) (time.Duration, *time.Duration, bool, error) {
		if id == "b" {
			return 0, nil, false, fakeAWSError{code: "NotFound", msg: "gone"}
		}
		return time.Millisecond, nil, false, nil
	})
	res := col.result("DeleteX")
	if res.Errors.Total != 1 {
		t.Errorf("Errors.Total = %d, want 1", res.Errors.Total)
	}
	if res.APILatency.Count != 2 {
		t.Errorf("APILatency.Count = %d, want 2", res.APILatency.Count)
	}
}
