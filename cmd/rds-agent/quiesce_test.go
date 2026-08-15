package main

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// fakeSession stands in for the held psql child. Closing it is what the engine
// treats as the end of the backup, so a test asserts on that rather than on any
// statement the release would have sent.
type fakeSession struct {
	mu       sync.Mutex
	executed []string
	closed   int
	execErr  error
	closeErr error
}

var _ engineSession = (*fakeSession)(nil)

func (f *fakeSession) Exec(_ context.Context, sql string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.executed = append(f.executed, sql)
	return f.execErr
}

func (f *fakeSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return f.closeErr
}

func (f *fakeSession) statements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.executed...)
}

func (f *fakeSession) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// newQuiesceEngine wires an engine whose backup sessions are the fake, and
// returns the commands each session was started with.
func newQuiesceEngine(t *testing.T, session *fakeSession) (*postgresEngine, *[]command) {
	t.Helper()
	started := &[]command{}
	engine := newTestEngine(t, (&recordingRunner{}).run)
	engine.startSess = func(_ context.Context, c command) (engineSession, error) {
		*started = append(*started, c)
		return session, nil
	}
	return engine, started
}

// The label rides the environment and is re-quoted by psql, so it never reaches
// a shell word or an argv another process can read.
func TestPostgresEngine_QuiesceStartsTheBackupOnAHeldSession(t *testing.T) {
	session := &fakeSession{}
	engine, started := newQuiesceEngine(t, session)

	if err := engine.Quiesce(context.Background(), "orders-db-pre-upgrade", time.Minute); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}

	if len(*started) != 1 {
		t.Fatalf("started %d sessions, want 1", len(*started))
	}
	if !slices.Contains((*started)[0].Env, "RDS_BACKUP_LABEL=orders-db-pre-upgrade") {
		t.Errorf("env %v does not carry the backup label", (*started)[0].Env)
	}
	statements := session.statements()
	if len(statements) != 1 || !strings.Contains(statements[0], "pg_backup_start") {
		t.Fatalf("statements = %q, want a pg_backup_start", statements)
	}
	// A spread checkpoint would hold the snapshot open for minutes.
	if !strings.Contains(statements[0], "fast => true") {
		t.Errorf("statement %q does not force an immediate checkpoint", statements[0])
	}
	if session.closes() != 0 {
		t.Error("the session was closed while the backup was meant to be held")
	}
}

func TestPostgresEngine_UnquiesceStopsTheBackupAndEndsTheSession(t *testing.T) {
	session := &fakeSession{}
	engine, _ := newQuiesceEngine(t, session)
	if err := engine.Quiesce(context.Background(), "orders-db-pre-upgrade", time.Minute); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}

	if err := engine.Unquiesce(context.Background()); err != nil {
		t.Fatalf("Unquiesce: %v", err)
	}

	statements := session.statements()
	if len(statements) != 2 || !strings.Contains(statements[1], "pg_backup_stop") {
		t.Fatalf("statements = %q, want a pg_backup_stop", statements)
	}
	if session.closes() != 1 {
		t.Errorf("closed %d times, want the session ended exactly once", session.closes())
	}
}

func TestCommandRegistry_QuiesceCarriesTheLabelAndDeadline(t *testing.T) {
	engine := &fakeEngine{}
	reply := newCommander(nil, newCommandRegistry(engine, &fakeStorage{}), 0).execute(context.Background(),
		handlers_rds.Command{
			CommandID: "cmd-5",
			Type:      handlers_rds.CommandQuiesce,
			Parameters: []handlers_rds.Parameter{
				{Name: handlers_rds.CommandParamQuiesceLabel, Value: "orders-db-pre-upgrade"},
				{Name: handlers_rds.CommandParamQuiesceDeadlineSeconds, Value: "360"},
			},
		})

	if reply.Status != handlers_rds.CommandStatusSucceeded {
		t.Fatalf("reply = %+v, want succeeded", reply)
	}
	if engine.label != "orders-db-pre-upgrade" || engine.hold != 6*time.Minute {
		t.Errorf("engine got %q/%s, want the command's label and deadline", engine.label, engine.hold)
	}
}

// A hold the guest chose the length of would not be the bound the control plane
// is relying on, so a missing or unreadable deadline is refused.
func TestCommandRegistry_QuiesceRejectsAnUnusableDeadline(t *testing.T) {
	for _, value := range []string{"", "soon", "0", "-30"} {
		engine := &fakeEngine{}
		reply := newCommander(nil, newCommandRegistry(engine, &fakeStorage{}), 0).execute(context.Background(),
			handlers_rds.Command{
				CommandID: "cmd-6",
				Type:      handlers_rds.CommandQuiesce,
				Parameters: []handlers_rds.Parameter{
					{Name: handlers_rds.CommandParamQuiesceLabel, Value: "orders-db-pre-upgrade"},
					{Name: handlers_rds.CommandParamQuiesceDeadlineSeconds, Value: value},
				},
			})

		if reply.Status != handlers_rds.CommandStatusFailed {
			t.Errorf("deadline %q: reply = %+v, want failed", value, reply)
		}
		if engine.label != "" {
			t.Errorf("deadline %q: the engine was quiesced anyway", value)
		}
	}
}

func TestCommandRegistry_UnquiesceReachesTheEngine(t *testing.T) {
	engine := &fakeEngine{}
	reply := newCommander(nil, newCommandRegistry(engine, &fakeStorage{}), 0).execute(context.Background(),
		handlers_rds.Command{CommandID: "cmd-7", Type: handlers_rds.CommandUnquiesce})

	if reply.Status != handlers_rds.CommandStatusSucceeded {
		t.Fatalf("reply = %+v, want succeeded", reply)
	}
	if !engine.released {
		t.Error("unquiesce did not reach the engine")
	}
}
