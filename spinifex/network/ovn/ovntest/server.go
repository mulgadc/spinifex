// Package ovntest boots an in-process OVN Northbound database for tests. It
// runs libovsdb's pure-Go OVSDB server (no ovsdb-server / ovn-northd C daemons)
// backed by an in-memory store, so higher layers can drive a real LiveClient
// against a real NB DB in milliseconds. It exercises intent and reconcile
// wiring plus OVSDB transaction semantics; datapath and NB->SB translation stay
// in the VM-level e2e suites.
package ovntest

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/ovn-kubernetes/libovsdb/database/inmemory"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/ovn-kubernetes/libovsdb/server"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
)

// nbSchema is the OVN Northbound schema subset covering the tables nbdb models.
// The client model (nbdb.FullDatabaseModel) is a subset of it and validates
// against it at construction.
//
//go:embed ovn-nb.ovsschema
var nbSchema []byte

// NB is a running in-process OVN Northbound server. Endpoint is the libovsdb
// dial string ("unix:/path/to/sock"). Shutdown is registered via t.Cleanup.
type NB struct {
	Endpoint string

	srv     *server.OvsdbServer
	sockdir string
}

// StartNB boots the in-memory NB server on a unix socket and returns once it is
// ready to accept connections. It fails the test on any setup error and
// registers cleanup. The socket lives under os.MkdirTemp (short path) to stay
// under the ~108 byte sun_path limit; t.TempDir would overflow in worktrees.
func StartNB(t testing.TB) *NB {
	t.Helper()

	clientModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		t.Fatalf("nbdb.FullDatabaseModel: %v", err)
	}

	var schema ovsdb.DatabaseSchema
	if err := json.Unmarshal(nbSchema, &schema); err != nil {
		t.Fatalf("parse ovn-nb.ovsschema: %v", err)
	}

	dbModel, errs := model.NewDatabaseModel(schema, clientModel)
	if len(errs) > 0 {
		t.Fatalf("model.NewDatabaseModel: %v", errs)
	}

	logger := logr.Discard()
	db := inmemory.NewDatabase(map[string]model.ClientDBModel{"OVN_Northbound": clientModel}, &logger)

	srv, err := server.NewOvsdbServer(db, &logger, dbModel)
	if err != nil {
		t.Fatalf("server.NewOvsdbServer: %v", err)
	}

	//nolint:usetesting // t.TempDir() yields a path too long for the unix socket
	// sun_path (108-byte limit); MkdirTemp("") keeps it short under /tmp.
	sockdir, err := os.MkdirTemp("", "ovnnb")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	sock := filepath.Join(sockdir, "nb.sock")

	go func() {
		// Serve blocks until Close; a post-Close Accept error is expected.
		_ = srv.Serve("unix", sock)
	}()

	nb := &NB{Endpoint: "unix:" + sock, srv: srv, sockdir: sockdir}
	t.Cleanup(nb.shutdown)

	if err := nb.waitReady(2 * time.Second); err != nil {
		t.Fatalf("NB server not ready: %v", err)
	}
	return nb
}

// waitReady polls until Serve has bound the listener or the deadline elapses.
func (nb *NB) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if nb.srv.Ready() {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return context.DeadlineExceeded
}

func (nb *NB) shutdown() {
	nb.srv.Close()
	_ = os.RemoveAll(nb.sockdir)
}
