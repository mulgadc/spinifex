package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// The staged payload this boot has to acknowledge once PostgreSQL has applied
// it. Nil on an attach boot, which makes the acknowledgement step a no-op.
type pendingBootstrap struct {
	payloadID    string
	vmGeneration int64
	dataVolumeID string
}

// Fetches the boot material and writes the handoff rds-init is blocked waiting
// for, then points the health probe at the assigned port.
func (a *Agent) bootstrap(ctx context.Context) error {
	var cfg *handlers_rds.GetDBBootstrapConfigOutput
	if err := retryObserved(ctx, "bootstrap fetch", func(ctx context.Context) error {
		fetched, err := a.cp.GetBootstrapConfig(ctx, a.id)
		if err != nil {
			return err
		}
		if fetched == nil {
			return fmt.Errorf("bootstrap fetch returned no config")
		}
		// A control plane predating the encrypted payload answers initialize with
		// nothing to initialise with. Retrying re-dials the queue group until an
		// upgraded node answers, so a mixed-version fleet heals within one boot.
		if fetched.Mode == handlers_rds.BootstrapModeInitialize &&
			(fetched.MasterUserPassword == nil || *fetched.MasterUserPassword == "") {
			return fmt.Errorf("bootstrap fetch returned mode=%s with no master password", fetched.Mode)
		}
		cfg = fetched
		return nil
	}, func(err error) {
		a.hb.setBootstrapFailure("bootstrap fetch", err)
	}); err != nil {
		return err
	}
	a.hb.clearBootstrapFailure()

	// The fetch mutates nothing, so a handoff that cannot be written is retried
	// against the same staged payload rather than against an attach response.
	if err := retryObserved(ctx, "bootstrap handoff", func(context.Context) error {
		return a.handoffWriter(a.cfg.HandoffDir, cfg)
	}, func(err error) {
		a.hb.setBootstrapFailure("bootstrap handoff", err)
	}); err != nil {
		return err
	}
	a.hb.clearBootstrapFailure()
	if cfg.Port > 0 {
		a.probe.setPort(int(cfg.Port))
	}
	if cfg.BootstrapPending && cfg.PayloadID != "" {
		a.pending = &pendingBootstrap{
			payloadID:    cfg.PayloadID,
			vmGeneration: cfg.VMGeneration,
			dataVolumeID: cfg.DataVolumeID,
		}
	}
	// Mode is logged but never branched on: rds-init decides whether to initdb
	// from the state of the datadir.
	slog.Info("rds-agent: bootstrap config delivered",
		"mode", cfg.Mode, "port", cfg.Port, "parameters", len(cfg.Parameters),
		"bootstrapPending", cfg.BootstrapPending)
	return nil
}

// The handoff files rds-init reads. They live on tmpfs, so nothing survives a
// reboot and the next boot re-fetches rather than reusing a stale password or a
// re-minted cert.
const (
	handoffEnvFile    = "bootstrap.env"
	handoffParamsFile = "parameters.conf"
	handoffCertFile   = "server.crt"
	handoffKeyFile    = "server.key"

	// Root-only: only rds-init reads these, and one holds the master password.
	handoffMode    = 0o600
	handoffDirMode = 0o700
)

// bootstrap.env is written last and renamed into place, so its appearance —
// which rds-init waits on — means the whole handoff is complete.
func writeHandoff(dir string, cfg *handlers_rds.GetDBBootstrapConfigOutput) error {
	if cfg.MasterUsername == "" {
		return fmt.Errorf("bootstrap config carries no master username")
	}
	// Half a handoff would leave rds-init deciding what to bind and who to admit,
	// and the only answers it could reach on its own are the wildcards this
	// contract exists to remove.
	if cfg.ListenAddress == "" {
		return fmt.Errorf("bootstrap config carries no listen address for the engine to bind")
	}
	if cfg.ClientCIDR == "" {
		return fmt.Errorf("bootstrap config carries no client CIDR to scope client authentication to")
	}
	if err := os.MkdirAll(dir, handoffDirMode); err != nil {
		return fmt.Errorf("create handoff dir %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, and this one may have
	// been created more permissively earlier in the boot.
	if err := os.Chmod(dir, handoffDirMode); err != nil {
		return fmt.Errorf("secure handoff dir %s: %w", dir, err)
	}

	params, err := renderParameters(cfg.Engine, cfg.Parameters)
	if err != nil {
		return err
	}
	if err := writeHandoffFile(dir, handoffParamsFile, params); err != nil {
		return err
	}
	// Cert and key are written as a pair; half of one would start the engine
	// with TLS configured against a key it does not have.
	if cfg.ServingCertificate != "" && cfg.ServingPrivateKey != "" {
		if err := writeHandoffFile(dir, handoffCertFile, cfg.ServingCertificate); err != nil {
			return err
		}
		if err := writeHandoffFile(dir, handoffKeyFile, cfg.ServingPrivateKey); err != nil {
			return err
		}
	}

	return writeHandoffFile(dir, handoffEnvFile, renderBootstrapEnv(cfg))
}

// Every value is single-quoted because rds-init sources this with `.`: an
// unquoted password holding a space, `$` or `;` would be word-split or executed.
func renderBootstrapEnv(cfg *handlers_rds.GetDBBootstrapConfigOutput) string {
	var b strings.Builder
	b.WriteString("# Written by rds-agent. Regenerated on every boot; edits are lost.\n")
	writeEnvLine(&b, "RDS_MODE", cfg.Mode)
	writeEnvLine(&b, "RDS_DB_INSTANCE_IDENTIFIER", cfg.DBInstanceIdentifier)
	writeEnvLine(&b, "RDS_MASTER_USERNAME", cfg.MasterUsername)
	writeEnvLine(&b, "RDS_LISTEN_ADDRESS", cfg.ListenAddress)
	writeEnvLine(&b, "RDS_CLIENT_CIDR", cfg.ClientCIDR)
	writeEnvLine(&b, "RDS_DATA_VOLUME_ID", cfg.DataVolumeID)
	writeEnvLine(&b, "RDS_DATA_VOLUME_SERIAL", cfg.DataVolumeSerial)
	writeEnvLine(&b, "RDS_VM_GENERATION", strconv.FormatInt(cfg.VMGeneration, 10))
	writeEnvLine(&b, "RDS_FORMAT_AUTHORIZED", strconv.FormatBool(cfg.FormatAuthorized))
	// Neither needs volume access, which is why the assertion is made here: the
	// agent runs before rds-datadir mounts the volume, so rds-init is the first
	// component that can compare it against the completion receipt on disk.
	writeEnvLine(&b, "RDS_BOOTSTRAP_PENDING", boolFlag(cfg.BootstrapPending))
	writeEnvLine(&b, "RDS_PAYLOAD_ID", cfg.PayloadID)
	// Present only in initialize mode: an attach fetch has no password to write
	// and rds-init must not find a stale one.
	if cfg.MasterUserPassword != nil {
		writeEnvLine(&b, "RDS_MASTER_PASSWORD", *cfg.MasterUserPassword)
	}
	if cfg.DBName != "" {
		writeEnvLine(&b, "RDS_DB_NAME", cfg.DBName)
	}
	if cfg.Port > 0 {
		writeEnvLine(&b, "RDS_PORT", strconv.FormatInt(cfg.Port, 10))
	}
	return b.String()
}

// rds-init compares against 1 rather than sourcing a Go boolean, so the two
// halves of the guard cannot drift on spelling.
func boolFlag(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func writeEnvLine(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString("=")
	b.WriteString(shellQuote(value))
	b.WriteString("\n")
}

// Single quotes suppress every shell expansion; an embedded quote is closed,
// escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// postgresql.conf syntax, which MariaDB's option files also parse. Values are
// quoted so a space or unit suffix survives. Names are the engine's startup
// spellings, not the customer's: the wrong one leaves the engine unable to boot.
func renderParameters(engineName string, params []handlers_rds.Parameter) (string, error) {
	engine, err := handlers_rds.LookupEngine(engineName)
	if err != nil {
		return "", fmt.Errorf("render the resolved parameters: %w", err)
	}
	var b strings.Builder
	b.WriteString("# Resolved parameter group, written by rds-agent.\n")
	for _, p := range params {
		if p.Name == "" {
			continue
		}
		fmt.Fprintf(&b, "%s = '%s'\n", engine.OptionFileName(p.Name), strings.ReplaceAll(p.Value, "'", "''"))
	}
	return b.String(), nil
}

// Atomic: a temp file in the same directory, renamed over the target. The mode
// is set at creation, so content is never briefly readable at the process umask.
func writeHandoffFile(dir, name, content string) error {
	path := filepath.Join(dir, name)
	tmp := path + ".new"

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, handoffMode)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	// A failed write must not leave the temp file behind holding a password.
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}
