package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const mariadbRDSInitScript = "../../scripts/images/rds-mariadb/rds-init"

func writeEngineLog(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "error.log")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write engine log: %v", err)
	}
	return path
}

// The whole point of the log: "not answering on its socket" is the same message
// for a server still recovering and for one that refused to start, and only the
// engine's own log separates them.
func TestMariaDBProbe_QuotesTheEngineLogWhenTheSocketIsSilent(t *testing.T) {
	cfg := mariadbTestProbeConfig(t)
	cfg.EngineErrorLog = writeEngineLog(t, strings.Join([]string{
		"2026-08-12  1:49:26 0 [Note] Starting MariaDB 11.8.0",
		"2026-08-12  1:49:26 0 [ERROR] InnoDB: Cannot allocate memory for the buffer pool",
		"2026-08-12  1:49:26 0 [ERROR] Plugin 'InnoDB' registration as a STORAGE ENGINE failed.",
		"",
	}, "\n"))
	writePid(t, cfg.EnginePidFile, os.Getpid())

	state, message := mariadbProbeState(cfg,
		func(context.Context, string, ...string) (int, string, error) { return 1, "", nil },
		func(int) bool { return true })(t.Context(), int64(cfg.EnginePort))

	if state != engineRecovering {
		t.Errorf("state = %v, want recovering", state)
	}
	if !strings.Contains(message, "Cannot allocate memory for the buffer pool") {
		t.Errorf("message = %q, want the engine's own refusal quoted", message)
	}
}

func TestMariaDBProbe_SurvivesAnAbsentEngineLog(t *testing.T) {
	cfg := mariadbTestProbeConfig(t)
	cfg.EngineErrorLog = filepath.Join(t.TempDir(), "does-not-exist.log")
	writePid(t, cfg.EnginePidFile, os.Getpid())

	state, message := mariadbProbeState(cfg,
		func(context.Context, string, ...string) (int, string, error) { return 1, "", nil },
		func(int) bool { return true })(t.Context(), int64(cfg.EnginePort))

	if state != engineRecovering {
		t.Errorf("state = %v, want recovering", state)
	}
	if message != "engine is not answering on its socket yet (startup or crash recovery)" {
		t.Errorf("message = %q, want the bare reason when there is no log to quote", message)
	}
}

// A client refused during the handshake is closed by the server without the
// server ever logging a cause, so the engine log alone leaves the failure
// unexplained and only the client's own stderr names it.
func TestMariaDBProbe_QuotesTheProbeClientWhenItIsRefused(t *testing.T) {
	cfg := mariadbTestProbeConfig(t)
	cfg.EngineErrorLog = writeEngineLog(t, "2026-08-12  4:32:39 148 [Warning] Aborted connection 148\n")
	writePid(t, cfg.EnginePidFile, os.Getpid())

	state, message := mariadbProbeState(cfg,
		func(context.Context, string, ...string) (int, string, error) {
			return 1, "ERROR 2026 (HY000): TLS/SSL error: self-signed certificate\n", nil
		},
		func(int) bool { return true })(t.Context(), int64(cfg.EnginePort))

	if state != engineRecovering {
		t.Errorf("state = %v, want recovering", state)
	}
	if !strings.Contains(message, "self-signed certificate") {
		t.Errorf("message = %q, want the probe client's own error quoted", message)
	}
	if !strings.Contains(message, "Aborted connection") {
		t.Errorf("message = %q, want the engine log kept alongside the client's error", message)
	}
}

func TestCollapseProbeStderr(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "folds to one line", in: "ERROR 2026:\n  TLS error\n", want: "ERROR 2026: TLS error"},
		{name: "empty stays empty", in: "   \n", want: ""},
		{
			name: "bounded",
			in:   strings.Repeat("y", mariadbProbeStderrMaxBytes+50),
			want: strings.Repeat("y", mariadbProbeStderrMaxBytes) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := collapseProbeStderr(tt.in); got != tt.want {
				t.Errorf("collapseProbeStderr() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A probe that says nothing must not append an empty clause: the reason reaches
// a customer, and a dangling "reported:" would read as truncated output.
func TestWithProbeStderrLeavesReasonAloneWhenTheClientIsSilent(t *testing.T) {
	const reason = "engine is not answering on its socket yet"
	if got := withProbeStderr(reason, "  \n "); got != reason {
		t.Errorf("withProbeStderr() = %q, want the reason unchanged", got)
	}
}

func TestEngineLogTail(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		want        string
		wantMissing string
	}{
		{
			name: "keeps only the last lines",
			body: "one\ntwo\nthree\nfour\nfive\nsix\n",
			want: "three | four | five | six",
		},
		{
			name: "skips blank lines",
			body: "alpha\n\n\nbeta\n\n",
			want: "alpha | beta",
		},
		{
			name: "empty file yields nothing",
			body: "",
			want: "",
		},
		{
			name:        "drops a line the byte cap cut in half",
			body:        strings.Repeat("x", mariadbErrorLogTailBytes) + "\nlast\n",
			want:        "last",
			wantMissing: "x",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := engineLogTail(writeEngineLog(t, tt.body))
			if got != tt.want {
				t.Errorf("engineLogTail() = %q, want %q", got, tt.want)
			}
			if tt.wantMissing != "" && strings.Contains(got, tt.wantMissing) {
				t.Errorf("engineLogTail() = %q, want no partial line", got)
			}
		})
	}
}

func TestEngineLogTailUnsetPath(t *testing.T) {
	if got := engineLogTail(""); got != "" {
		t.Errorf("engineLogTail(\"\") = %q, want empty", got)
	}
}

// The agent reads a path the image writes, and nothing at runtime would notice
// the two drifting apart: the log would simply always be missing and every
// refusal would go back unexplained again.
func TestMariaDBErrorLogPathMatchesTheImage(t *testing.T) {
	raw, err := os.ReadFile(mariadbRDSInitScript)
	if err != nil {
		t.Fatalf("read rds-init: %v", err)
	}
	script := string(raw)

	logDir := shellDefault(t, script, "LOG_DIR", "RDS_LOG_DIR")
	engineLog := shellDefault(t, script, "ENGINE_LOG", "RDS_ENGINE_LOG")
	want := strings.Replace(engineLog, "${LOG_DIR}", logDir, 1)

	if got := engineLayouts[engineMariaDB].errorLog; got != want {
		t.Errorf("layout errorLog = %q, but rds-init writes log_error = %q", got, want)
	}
}

// Pulls the fallback out of a `NAME="${OVERRIDE:-fallback}"` assignment.
func shellDefault(t *testing.T, script, name, override string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + name + `="\$\{` + override + `:-(.*)\}"$`)
	m := re.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("rds-init has no %s assignment defaulting from %s", name, override)
	}
	return m[1]
}

func TestWithEngineLogTailLeavesReasonAloneWithoutALog(t *testing.T) {
	const reason = "engine is not answering on its socket yet"
	if got := withEngineLogTail(reason, ""); got != reason {
		t.Errorf("withEngineLogTail() = %q, want the reason unchanged", got)
	}
}
