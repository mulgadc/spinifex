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
	// The status an instance holds for the length of a snapshot, from before the
	// quiesce to after the release.
	dbInstanceBackingUp = "backing-up"

	// How often the watcher samples the status. The hold has lasted 0.3 to 1.3s,
	// so this sees it with time left to send a rotation into it.
	backingUpSampleEvery = 50 * time.Millisecond

	// One window: the reconciler's pass that fires it, the backup itself, and the
	// minute boundary a later attempt has to wait for.
	backupOverlapAttemptTimeout = 5 * time.Minute

	// The status is sampled, so a short hold can still slip between two samples.
	// Each further attempt opens a fresh window.
	maxBackupOverlapAttempts = 3

	// How often an attempt still waiting for backing-up looks for a snapshot that
	// landed without the watcher seeing the status.
	backupLandedCheckEvery = time.Second

	// A probe table only this check writes to, so its DDL and commit say nothing
	// about any row the rest of the test reads.
	backupProbeTable = "e2e_backup_probe"
)

// How one window went for the rotation sent into it.
type overlapAttempt int

const (
	overlapped overlapAttempt = iota
	// The backup landed and no sample read backing-up, so nothing was sent.
	missedStatus
	// A rotation was sent, but no describe sent after it read backing-up.
	lateSend
)

func (a overlapAttempt) String() string {
	switch a {
	case overlapped:
		return "a rotation overlapped the backup"
	case missedStatus:
		return "the backup landed without the watcher seeing backing-up"
	default:
		return "the rotation was not seen inside backing-up"
	}
}

// Sends a rotation as soon as the instance reports backing-up for an automated
// backup, until one is seen inside it. conn is kept on whichever password is
// live, so later subtests connect with it.
func rotateDuringAutomatedBackup(t *testing.T, f *Fixture, client harness.SSHTarget, id string, conn *harness.MariaDBConn) {
	t.Helper()
	// Put back once done, so no later subtest can be interrupted by a retry of
	// a backup that failed inside the window this opened.
	t.Cleanup(func() { pinBackupWindowClearOfTheRun(t, f, id) })

	var lastOpened time.Time
	_, ok := retryUntilOverlap(maxBackupOverlapAttempts, func(attempt int) overlapAttempt {
		result := rotateIntoOneBackup(t, f, client, id, conn, &lastOpened)
		if result != overlapped {
			t.Logf("attempt %d: %s of %s; opening another window", attempt, result, id)
		}
		return result
	})
	if !ok {
		t.Fatalf("no rotation overlapped an automated backup of %s in %d windows", id, maxBackupOverlapAttempts)
	}
}

// Runs attempts until one overlaps, and reports which one did.
func retryUntilOverlap(attempts int, attempt func(int) overlapAttempt) (int, bool) {
	for n := 1; n <= attempts; n++ {
		if attempt(n) == overlapped {
			return n, true
		}
	}
	return 0, false
}

// Opens a window, sends one rotation once the instance reports backing-up, and
// waits for the backup to land before saying how the rotation fell.
func rotateIntoOneBackup(t *testing.T, f *Fixture, client harness.SSHTarget, id string,
	conn *harness.MariaDBConn, lastOpened *time.Time) overlapAttempt {
	t.Helper()
	before, err := automatedSnapshots(f, id)
	require.NoError(t, err)
	beforeIDs := automatedSnapshotIDs(before)

	watch := watchStatus(f, id)
	*lastOpened = openBackupWindowNow(t, f, id, *lastOpened)
	deadline := time.Now().Add(backupOverlapAttemptTimeout)

	var sent *time.Time
	nextLandedCheck := time.Now().Add(backupLandedCheckEvery)
	for {
		require.True(t, time.Now().Before(deadline),
			"no automated backup of %s finished within %s of its window opening", id, backupOverlapAttemptTimeout)
		if watch.latest() == dbInstanceBackingUp {
			issued, err := rotateOnce(t, f, client, id, conn)
			sent = &issued
			t.Logf("rotation sent at %s while %s reported backing-up (err=%v)", issued.Format(time.RFC3339Nano), id, err)
			break
		}
		if time.Now().After(nextLandedCheck) {
			if newAutomatedSnapshot(t, f, id, beforeIDs) {
				break
			}
			nextLandedCheck = time.Now().Add(backupLandedCheckEvery)
		}
		time.Sleep(5 * time.Millisecond)
	}

	for watch.latest() == dbInstanceBackingUp || !newAutomatedSnapshot(t, f, id, beforeIDs) {
		require.True(t, time.Now().Before(deadline),
			"no automated backup of %s finished within %s of its window opening", id, backupOverlapAttemptTimeout)
		time.Sleep(backupLandedCheckEvery)
	}
	watch.stop()
	if err := watch.err(); err != nil {
		t.Logf("the status watcher missed samples; the last describe failed with: %v", err)
	}

	harness.WaitForDBInstanceAvailable(t, f.AWS, id, harness.WithPoll(2*time.Second),
		harness.WithTimeout(2*time.Minute))
	assertTheEngineIsReleased(t, client, *conn)

	result := classifyOverlapAttempt(watch.all(), sent)
	if result == overlapped {
		t.Logf("rotation in flight from %s overlapped the backup", sent.Format(time.RFC3339Nano))
	}
	return result
}

// Classifies a window from its samples and the rotation's send time, nil when
// no rotation was sent.
func classifyOverlapAttempt(samples []statusSample, sent *time.Time) overlapAttempt {
	switch {
	case sent == nil:
		return missedStatus
	case rotationOverlapped(samples, *sent):
		return overlapped
	default:
		return lateSend
	}
}

// The rotation is only sent once a sample reads backing-up, and the status
// holds it once per backup, so the first describe sent at or after the rotation
// reading it shows the status was held when the rotation went out.
func rotationOverlapped(samples []statusSample, sent time.Time) bool {
	for _, s := range samples {
		if !s.sent.Before(sent) {
			return s.status == dbInstanceBackingUp
		}
	}
	return false
}

// Opens the backup window on the current minute. When the previous attempt's
// window opened on this one, it first waits for the next minute: a window fires
// once, and the write that opens it is what wakes the reconciler to fire it.
func openBackupWindowNow(t *testing.T, f *Fixture, id string, previous time.Time) time.Time {
	t.Helper()
	if next := previous.Add(time.Minute); time.Now().Before(next) {
		time.Sleep(time.Until(next))
	}
	opens := time.Now().UTC().Truncate(time.Minute)
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

// Rotates the master password to whichever of the two it is not on, checks the
// rotation is whole, and returns when it was sent.
func rotateOnce(t *testing.T, f *Fixture, client harness.SSHTarget, id string, conn *harness.MariaDBConn) (time.Time, error) {
	t.Helper()
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
	assertRotationIsWhole(t, client, conn, next, err)
	return issued, err
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
// that lasts well under a second.
type statusWatch struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	samples []statusSample
	lastErr error
}

// sent is when the describe went out, so a reply that was already in flight
// when something else happened is never read as coming after it.
type statusSample struct {
	sent   time.Time
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
			sent := time.Now()
			instance, err := harness.DescribeDBInstance(f.AWS, id)
			w.mu.Lock()
			if err != nil {
				w.lastErr = err
			} else {
				w.samples = append(w.samples, statusSample{sent: sent, status: aws.StringValue(instance.DBInstanceStatus)})
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

func (w *statusWatch) all() []statusSample {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.samples)
}
