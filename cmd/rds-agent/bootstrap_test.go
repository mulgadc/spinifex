package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

func TestWriteHandoff_WritesEveryFileRootOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "handoff")
	password := "s3cret"

	err := writeHandoff(dir, &handlers_rds.GetDBBootstrapConfigOutput{
		Mode:               handlers_rds.BootstrapModeInitialize,
		Engine:             "postgres",
		MasterUsername:     "master",
		MasterUserPassword: &password,
		DBName:             "appdb",
		Port:               5433,
		DataVolumeID:       "vol-data-01",
		DataVolumeSerial:   "voldata01",
		VMGeneration:       4,
		FormatAuthorized:   true,
		Parameters: []handlers_rds.Parameter{
			{Name: "max_connections", Value: "200"},
			{Name: "shared_buffers", Value: "256MB"},
		},
		ServingCertificate: "CERT",
		ServingPrivateKey:  "KEY",
	})
	if err != nil {
		t.Fatalf("writeHandoff: %v", err)
	}

	env := readFile(t, filepath.Join(dir, handoffEnvFile))
	for _, want := range []string{
		"RDS_MODE='initialize'",
		"RDS_MASTER_USERNAME='master'",
		"RDS_MASTER_PASSWORD='s3cret'",
		"RDS_DB_NAME='appdb'",
		"RDS_PORT='5433'",
		"RDS_DATA_VOLUME_ID='vol-data-01'",
		"RDS_DATA_VOLUME_SERIAL='voldata01'",
		"RDS_VM_GENERATION='4'",
		"RDS_FORMAT_AUTHORIZED='true'",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("handoff env missing %s:\n%s", want, env)
		}
	}

	params := readFile(t, filepath.Join(dir, handoffParamsFile))
	if !strings.Contains(params, "max_connections = '200'") || !strings.Contains(params, "shared_buffers = '256MB'") {
		t.Errorf("parameters.conf = %q, want both resolved settings", params)
	}
	if got := readFile(t, filepath.Join(dir, handoffCertFile)); got != "CERT" {
		t.Errorf("server.crt = %q, want CERT", got)
	}
	if got := readFile(t, filepath.Join(dir, handoffKeyFile)); got != "KEY" {
		t.Errorf("server.key = %q, want KEY", got)
	}

	// The handoff holds a master password and a private key; anything readable
	// beyond root is a disclosure to every process in the guest.
	if mode := statMode(t, dir); mode != handoffDirMode {
		t.Errorf("handoff dir mode = %#o, want %#o", mode, handoffDirMode)
	}
	for _, name := range []string{handoffEnvFile, handoffParamsFile, handoffCertFile, handoffKeyFile} {
		if mode := statMode(t, filepath.Join(dir, name)); mode != handoffMode {
			t.Errorf("%s mode = %#o, want %#o", name, mode, handoffMode)
		}
	}
}

// Attach mode carries no password, and the key must be absent rather than
// present and empty — rds-init reads an empty value as "no password" only by
// accident, and a later boot would inherit it.
func TestWriteHandoff_AttachOmitsPassword(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "handoff")
	if err := writeHandoff(dir, &handlers_rds.GetDBBootstrapConfigOutput{
		Mode:           handlers_rds.BootstrapModeAttach,
		Engine:         "postgres",
		MasterUsername: "master",
		Port:           5432,
	}); err != nil {
		t.Fatalf("writeHandoff: %v", err)
	}

	env := readFile(t, filepath.Join(dir, handoffEnvFile))
	if strings.Contains(env, "RDS_MASTER_PASSWORD") {
		t.Errorf("handoff env = %q, want no RDS_MASTER_PASSWORD key", env)
	}
	for _, want := range []string{
		"RDS_DATA_VOLUME_ID=''",
		"RDS_DATA_VOLUME_SERIAL=''",
		"RDS_VM_GENERATION='0'",
		"RDS_FORMAT_AUTHORIZED='false'",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("handoff env missing fail-closed field %s: %q", want, env)
		}
	}
}

// The env file is sourced by a shell, so a password holding shell metacharacters
// must survive as data rather than being split, expanded or executed.
func TestWriteHandoff_QuotesShellMetacharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "handoff")
	password := `a b$(id);'"\` + "`whoami`"

	if err := writeHandoff(dir, &handlers_rds.GetDBBootstrapConfigOutput{
		Mode:               handlers_rds.BootstrapModeInitialize,
		Engine:             "postgres",
		MasterUsername:     "master",
		MasterUserPassword: &password,
		Port:               5432,
	}); err != nil {
		t.Fatalf("writeHandoff: %v", err)
	}

	env := readFile(t, filepath.Join(dir, handoffEnvFile))
	line := findLine(t, env, "RDS_MASTER_PASSWORD=")
	if got := shellSource(t, line, "RDS_MASTER_PASSWORD"); got != password {
		t.Errorf("sourced password = %q, want %q (line was %s)", got, password, line)
	}
}

// A serving cert with no key would have rds-init configure TLS against a key
// that is not there; the pair is delivered together or not at all.
func TestWriteHandoff_SkipsHalfATLSPair(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "handoff")
	if err := writeHandoff(dir, &handlers_rds.GetDBBootstrapConfigOutput{
		Mode:               handlers_rds.BootstrapModeAttach,
		Engine:             "postgres",
		MasterUsername:     "master",
		Port:               5432,
		ServingCertificate: "CERT",
	}); err != nil {
		t.Fatalf("writeHandoff: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, handoffCertFile)); !os.IsNotExist(err) {
		t.Errorf("server.crt exists without a key; stat err = %v", err)
	}
}

// A config with no master username cannot bootstrap anything, and writing it
// would only move the failure into rds-init after the boot has committed to it.
func TestWriteHandoff_RejectsConfigWithNoMasterUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "handoff")
	err := writeHandoff(dir, &handlers_rds.GetDBBootstrapConfigOutput{
		Mode:   handlers_rds.BootstrapModeAttach,
		Engine: "postgres",
		Port:   5432,
	})
	if err == nil {
		t.Fatal("writeHandoff accepted a config with no master username, want an error")
	}
	if _, statErr := os.Stat(filepath.Join(dir, handoffEnvFile)); !os.IsNotExist(statErr) {
		t.Errorf("handoff env was written despite the rejection; stat err = %v", statErr)
	}
}

// A directory left over from an earlier boot at a looser mode must be tightened,
// not accepted as it is found.
func TestWriteHandoff_TightensExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "handoff")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}

	if err := writeHandoff(dir, &handlers_rds.GetDBBootstrapConfigOutput{
		Mode:           handlers_rds.BootstrapModeAttach,
		Engine:         "postgres",
		MasterUsername: "master",
		Port:           5432,
	}); err != nil {
		t.Fatalf("writeHandoff: %v", err)
	}
	if mode := statMode(t, dir); mode != handoffDirMode {
		t.Errorf("handoff dir mode = %#o, want it tightened to %#o", mode, handoffDirMode)
	}
}

func TestShellQuote_EscapesEmbeddedQuotes(t *testing.T) {
	for _, in := range []string{"", "plain", "it's", `''`, "new\nline", `$HOME`} {
		if got := shellSource(t, "V="+shellQuote(in), "V"); got != in {
			t.Errorf("shellQuote(%q) sourced back as %q", in, got)
		}
	}
}

// shellSource evaluates one KEY=value assignment in a real shell and returns
// what the variable ends up holding. The quoting is only correct if a shell
// agrees, so the test asks one instead of re-implementing its rules.
func shellSource(t *testing.T, line, name string) string {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", line+"\nprintf %s \"$"+name+"\"\n").Output()
	if err != nil {
		t.Fatalf("sourcing %q: %v", line, err)
	}
	return string(out)
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func findLine(t *testing.T, body, prefix string) string {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no line starting %q in:\n%s", prefix, body)
	return ""
}

// The include rds-init installs is what the engine starts on, so the spelling
// written here is the one that has to boot.
func TestRenderParameters_UsesTheEngineStartupSpelling(t *testing.T) {
	params := []handlers_rds.Parameter{
		{Name: "time_zone", Value: "SYSTEM"},
		{Name: "max_connections", Value: "85"},
	}

	rendered, err := renderParameters("mariadb", params)
	if err != nil {
		t.Fatalf("renderParameters: %v", err)
	}
	if !strings.Contains(rendered, "default_time_zone = 'SYSTEM'\n") {
		t.Errorf("time_zone was not written under its startup spelling:\n%s", rendered)
	}
	if strings.Contains(rendered, "\ntime_zone = ") {
		t.Errorf("time_zone reached the option file, which mariadbd refuses:\n%s", rendered)
	}
	if !strings.Contains(rendered, "max_connections = '85'\n") {
		t.Errorf("an unmapped parameter was not written unchanged:\n%s", rendered)
	}
}

func TestRenderParameters_PostgresKeepsEveryName(t *testing.T) {
	rendered, err := renderParameters("postgres", []handlers_rds.Parameter{
		{Name: "shared_buffers", Value: "262144"},
	})
	if err != nil {
		t.Fatalf("renderParameters: %v", err)
	}
	if !strings.Contains(rendered, "shared_buffers = '262144'\n") {
		t.Errorf("postgres name was rewritten:\n%s", rendered)
	}
}

// Failing closed beats writing a file the engine may refuse: the bad include
// would already be on the data volume by the time the server rejected it.
func TestRenderParameters_RefusesAnUnknownEngine(t *testing.T) {
	if _, err := renderParameters("", nil); err == nil {
		t.Fatal("renderParameters accepted a config naming no engine, want an error")
	}
}
