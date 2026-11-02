//go:build e2e

package rds

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// The status an instance holds for the length of a snapshot, from the quiesce
	// to the release.
	dbInstanceBackingUp = "backing-up"

	// How often the watcher samples the status. The hold lasts about a second, so
	// the harness's 10s status interval would step straight over it.
	backingUpSampleEvery = 100 * time.Millisecond

	// One window: the reconciler's pass that fires it, the backup itself, and the
	// minute boundary a later attempt has to wait for.
	backupOverlapAttemptTimeout = 5 * time.Minute

	// The hold is short and the rotations are discrete, so one backup can finish
	// between two of them. Each further attempt opens a fresh window.
	maxBackupOverlapAttempts = 3

	// A probe table only this check writes to, so its DDL and commit say nothing
	// about any row the rest of the test reads.
	backupProbeTable = "e2e_backup_probe"
)

// Rotates the master password back and forth through an automated backup until
// one rotation is seen in flight while the instance reports backing-up. conn is
// kept on whichever password is live, so later subtests connect with it.
func rotateDuringAutomatedBackup(t *testing.T, f *Fixture, client harness.SSHTarget, id string, conn *harness.MariaDBConn) {
	t.Helper()
	// Put back once done, so no later subtest can be interrupted by a retry of
	// a backup that failed inside the window this opened.
	t.Cleanup(func() { pinBackupWindowClearOfTheRun(t, f, id) })

	var lastOpened time.Time
	for attempt := 1; attempt <= maxBackupOverlapAttempts; attempt++ {
		before, err := automatedSnapshots(f, id)
		require.NoError(t, err)
		beforeIDs := automatedSnapshotIDs(before)

		lastOpened = openBackupWindowNow(t, f, id, lastOpened)
		watch := watchStatus(f, id)
		overlapped := rotateUntilTheBackupLands(t, f, client, id, conn, beforeIDs, watch)
		watch.stop()
		if err := watch.err(); err != nil {
			t.Logf("the status watcher missed samples; the last describe failed with: %v", err)
		}

		harness.WaitForDBInstanceAvailable(t, f.AWS, id, harness.WithPoll(2*time.Second),
			harness.WithTimeout(2*time.Minute))
		assertTheEngineIsReleased(t, client, *conn)

		if overlapped {
			return
		}
		t.Logf("attempt %d: the backup of %s finished between two rotations; opening another window", attempt, id)
	}
	t.Fatalf("no rotation overlapped an automated backup of %s in %d windows", id, maxBackupOverlapAttempts)
}

// Opens the backup window on the current minute, or on the next one when the
// previous attempt's window already opened on this one: a window fires once,
// and only for a backup older than its opening.
func openBackupWindowNow(t *testing.T, f *Fixture, id string, previous time.Time) time.Time {
	t.Helper()
	opens := time.Now().UTC().Truncate(time.Minute)
	if !opens.After(previous) {
		opens = previous.Add(time.Minute)
	}
	window := dailyWindowAt(opens, 30*time.Minute)
	harness.Step(t, "Opening the backup window of %q at %s", id, window)
	_, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
		DBInstanceIdentifier:  aws.String(id),
		PreferredBackupWindow: aws.String(window),
		ApplyImmediately:      aws.Bool(true),
	})
	require.NoError(t, err, "modify-db-instance backup window")
	return opens
}

// Rotates until the backup has produced its snapshot and the instance has left
// backing-up, and reports whether any rotation was in flight while it was in it.
func rotateUntilTheBackupLands(t *testing.T, f *Fixture, client harness.SSHTarget, id string,
	conn *harness.MariaDBConn, beforeIDs []string, watch *statusWatch) bool {
	t.Helper()
	deadline := time.Now().Add(backupOverlapAttemptTimeout)
	overlapped := false
	for {
		require.True(t, time.Now().Before(deadline),
			"no automated backup of %s finished within %s of its window opening", id, backupOverlapAttemptTimeout)

		next := mariaRotatedPassword
		if conn.Password == mariaRotatedPassword {
			next = mariaBackupRotationPassword
		}
		issued := time.Now()
		_, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
			DBInstanceIdentifier: aws.String(id),
			MasterUserPassword:   aws.String(next),
			ApplyImmediately:     aws.Bool(true),
		})
		answered := time.Now()
		if watch.sawBetween(dbInstanceBackingUp, issued, answered) {
			overlapped = true
			t.Logf("rotation in flight %s-%s overlapped the backup (err=%v)",
				issued.Format(time.RFC3339Nano), answered.Format(time.RFC3339Nano), err)
		}
		assertRotationIsWhole(t, client, conn, next, err)

		if watch.latest() != dbInstanceBackingUp && newAutomatedSnapshot(t, f, id, beforeIDs) {
			return overlapped
		}
	}
}

// An accepted rotation leaves only the new password working; a refused one
// leaves only the old. Anything else is a rotation the caller cannot reason about.
func assertRotationIsWhole(t *testing.T, client harness.SSHTarget, conn *harness.MariaDBConn, next string, err error) {
	t.Helper()
	rotated := *conn
	rotated.Password = next
	live, retired := rotated, *conn
	if err != nil {
		harness.AssertAWSError(t, err, "InvalidDBInstanceState")
		live, retired = *conn, rotated
	}

	assert.Equal(t, mariaNote, mariaValue(t, client, live,
		fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", mariaTable)),
		"the password the API's answer implies must be the one that works (modify err=%v)", err)
	out, loginErr := harness.TryMariaDB(client, retired, "SELECT 1;")
	require.Error(t, loginErr, "the password the API's answer rules out still connects (modify err=%v): %s", err, out)
	require.Contains(t, out, "Access denied", "the other password must be refused by the engine, not the network")

	*conn = live
}

// A held BACKUP STAGE blocks DDL and commits, so a statement doing both
// completing is what shows the hold was released rather than left to expire.
func assertTheEngineIsReleased(t *testing.T, client harness.SSHTarget, conn harness.MariaDBConn) {
	t.Helper()
	harness.MariaDB(t, client, conn, fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (id int primary key); REPLACE INTO %s VALUES (1);",
		backupProbeTable, backupProbeTable))
}

func newAutomatedSnapshot(t *testing.T, f *Fixture, id string, beforeIDs []string) bool {
	t.Helper()
	snapshots, err := automatedSnapshots(f, id)
	require.NoError(t, err)
	for _, snapshot := range snapshots {
		if !slices.Contains(beforeIDs, aws.StringValue(snapshot.DBSnapshotIdentifier)) &&
			aws.StringValue(snapshot.Status) == harness.DBSnapshotAvailable {
			return true
		}
	}
	return false
}

func pinBackupWindowClearOfTheRun(t *testing.T, f *Fixture, id string) {
	t.Helper()
	backup, _ := windowsClearOfTheRun()
	_, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
		DBInstanceIdentifier:  aws.String(id),
		PreferredBackupWindow: aws.String(backup),
		ApplyImmediately:      aws.Bool(true),
	})
	assert.NoError(t, err, "move the backup window of %s clear of the run again", id)
}

// Samples an instance's status on its own goroutine, fast enough to see a hold
// that lasts about a second.
type statusWatch struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	samples []statusSample
	lastErr error
}

type statusSample struct {
	at     time.Time
	status string
}

func watchStatus(f *Fixture, id string) *statusWatch {
	ctx, cancel := context.WithCancel(context.Background())
	w := &statusWatch{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(backingUpSampleEvery)
		defer ticker.Stop()
		for {
			instance, err := harness.DescribeDBInstance(f.AWS, id)
			w.mu.Lock()
			if err != nil {
				w.lastErr = err
			} else {
				w.samples = append(w.samples, statusSample{at: time.Now(), status: aws.StringValue(instance.DBInstanceStatus)})
			}
			w.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return w
}

func (w *statusWatch) stop() {
	w.cancel()
	<-w.done
}

func (w *statusWatch) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastErr
}

func (w *statusWatch) latest() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.samples) == 0 {
		return ""
	}
	return w.samples[len(w.samples)-1].status
}

func (w *statusWatch) sawBetween(status string, from, to time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.samples {
		if s.status == status && !s.at.Before(from) && !s.at.After(to) {
			return true
		}
	}
	return false
}
