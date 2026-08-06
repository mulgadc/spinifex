package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mulgadc/spinifex/internal/rdsgw"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// The completion receipt rds-init writes once the master role is durably
// applied. Deliberately outside PGDATA: clear_unfinished_datadir does an rm -rf
// of PGDATA, so keeping the receipt out of it means no initdb-adjacent path can
// touch it and the datadir stays a byte-for-byte stock one.
const (
	receiptRelDir  = ".spinifex-rds/bootstrap"
	receiptFile    = "receipt.env"
	receiptPayload = "RDS_RECEIPT_PAYLOAD_ID"
	receiptDBID    = "RDS_RECEIPT_DB_INSTANCE_IDENTIFIER"
)

func (a *Agent) receiptPath() string {
	return filepath.Join(a.cfg.DataMount, receiptRelDir, receiptFile)
}

// Confirms the initial bootstrap, which is what destroys the staged ciphertext.
// A no-op on the attach path, and deliberately synchronous: it inherits the
// cancellation semantics register, bootstrap and the data-mount wait already
// share, and blocking the stateful loops behind it is correct because on a first
// boot the engine is not bootstrapped until rds-init has finished anyway.
//
// It does not wait for the engine to be healthy. The receipt already proves the
// master role was durably applied, and acknowledging early shortens the window
// in which the ciphertext exists.
func (a *Agent) acknowledgeBootstrap(ctx context.Context) error {
	pending := a.pending
	if pending == nil {
		return nil
	}

	// Polled rather than waited on: rds-init runs after this agent registered,
	// so the file appears some time into the boot. There is no give-up deadline
	// here — the control plane's bootstrap timeout already fails the instance,
	// and a second one in the guest would only give the two a way to disagree.
	if err := retryObserved(ctx, "bootstrap receipt", func(context.Context) error {
		return a.checkBootstrapReceipt(pending)
	}, func(err error) {
		// An absent receipt is the expected state for the whole initdb window,
		// so reporting it would put a failure on every healthy create's beat and
		// then into the reason a timed-out create records.
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		a.hb.setBootstrapFailure("bootstrap receipt", err)
	}); err != nil {
		return err
	}
	a.hb.clearBootstrapFailure()

	if err := retryObserved(ctx, "bootstrap acknowledgement", func(ctx context.Context) error {
		return terminalOnDenial(a.cp.AcknowledgeBootstrap(ctx, a.id, pending))
	}, func(err error) {
		a.hb.setBootstrapFailure("bootstrap acknowledgement", err)
	}); err != nil {
		if !errors.Is(err, errRetryTerminal) {
			return err
		}
		// The staged payload will never be delivered to this VM, and blocking the
		// command poller and the parameter guard on that would leave a serving
		// database no one can administer. The reason rides the heartbeat instead.
		slog.ErrorContext(ctx, "rds-agent: the initial bootstrap cannot be acknowledged",
			"payloadId", pending.payloadID, "err", err)
		return nil
	}
	a.hb.clearBootstrapFailure()

	// AcknowledgedAt on the record is the durable audit trail, so the receipt is
	// removed rather than archived: keeping it would only grow the surface of
	// stale receipts riding along in data-volume snapshots. A removal failure is
	// harmless, since a later boot fetches attach and never consults one.
	if err := os.Remove(a.receiptPath()); err != nil && !os.IsNotExist(err) {
		slog.WarnContext(ctx, "rds-agent: could not remove the bootstrap receipt",
			"receipt", a.receiptPath(), "err", err)
	}
	slog.InfoContext(ctx, "rds-agent: initial bootstrap acknowledged", "payloadId", pending.payloadID)
	return nil
}

// The control plane resolved these from its own record — a superseded
// generation, a payload it never staged, a data volume that is not this
// instance's. Asking again cannot change any of them.
func terminalOnDenial(err error) error {
	var apiErr *rdsgw.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case awserrors.ErrorAccessDenied, awserrors.ErrorInvalidParameterValue:
		return fmt.Errorf("%w: %w", errRetryTerminal, err)
	}
	return err
}

// A receipt naming another payload or another DB instance is treated as absent.
// The receipt lives on the data volume, so it rides along in every snapshot of
// it and a restored instance's volume carries the source instance's receipt.
func (a *Agent) checkBootstrapReceipt(pending *pendingBootstrap) error {
	path := a.receiptPath()
	values, err := readReceipt(path)
	if err != nil {
		return err
	}
	if got := values[receiptPayload]; got != pending.payloadID {
		return fmt.Errorf("%s names bootstrap payload %q, not %q", path, got, pending.payloadID)
	}
	if got := values[receiptDBID]; got != a.id.DBInstanceIdentifier {
		return fmt.Errorf("%s names DB instance %q, not %q", path, got, a.id.DBInstanceIdentifier)
	}
	return nil
}

// KEY=value, the one format both readers can parse: rds-init in POSIX shell
// with no guaranteed jq, and this.
func readReceipt(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return values, nil
}
