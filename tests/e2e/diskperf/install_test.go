//go:build e2e

package diskperf

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A transient apt failure on the first attempts must not end the run: the
// install is retried with a growing wait and the run goes on once fio is in.
func TestInstallFioRetriesATransientAptFailure(t *testing.T) {
	calls := 0
	var waits []time.Duration
	err := installFioWith(func(string) (string, error) {
		calls++
		if calls < 3 {
			return "E: Could not get lock /var/lib/dpkg/lock-frontend", fmt.Errorf("exit status %d", exitAptInstall)
		}
		return "", nil
	}, func(d time.Duration) { waits = append(waits, d) })

	if err != nil {
		t.Fatalf("install failed although the third attempt succeeded: %v", err)
	}
	if calls != 3 {
		t.Errorf("ran the install %d times, want 3", calls)
	}
	if want := []time.Duration{fioInstallBackoff, 2 * fioInstallBackoff}; !slices.Equal(waits, want) {
		t.Errorf("waited %v between attempts, want %v", waits, want)
	}
}

// An apt that never recovers is still reported as an infra fault, with the
// last attempt's exit status and output, after a bounded number of attempts.
func TestInstallFioGivesUpAsAnInfraFault(t *testing.T) {
	calls := 0
	sleeps := 0
	aptErr := errors.New("exit status 10")
	err := installFioWith(func(string) (string, error) {
		calls++
		return "Temporary failure resolving 'archive.ubuntu.com'", aptErr
	}, func(time.Duration) { sleeps++ })

	if err == nil {
		t.Fatal("install reported success although every attempt failed")
	}
	if calls != fioInstallAttempts || sleeps != fioInstallAttempts-1 {
		t.Errorf("made %d attempts with %d waits, want %d and %d", calls, sleeps, fioInstallAttempts, fioInstallAttempts-1)
	}
	if !errors.Is(err, aptErr) {
		t.Errorf("error lost the apt exit status: %v", err)
	}
	for _, want := range []string{"infra fault, not a performance result", "Temporary failure resolving"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}
