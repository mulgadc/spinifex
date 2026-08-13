//go:build e2e

package harness

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
)

// A DB instance's endpoint only ever has a private address (public access is rejected
// PubliclyAccessible), and nothing on the runner routes into a customer subnet.
// So the client that proves a customer can reach the database has to be a VM in
// that subnet, driven over SSH — not psql on the machine running the test.
//
// The client is an Ubuntu gold image with both engines' clients installed at
// boot, not an engine AMI: those images carry neither sshd nor cloud-init, so a
// VM launched from one would come up unreachable.
const (
	rdsClientUser = "ubuntu"

	// Where the boot-time install reports itself. The exit-code file is written
	// from an EXIT trap so the poller can always read a definitive answer
	// instead of inferring one from a missing binary.
	rdsClientStatusFile   = "/tmp/rds-client-setup.status"
	rdsClientExitCodeFile = "/tmp/rds-client-setup.exitcode"

	// The cluster CA, pushed into the guest so a verify-full connection has a
	// trust root. The engine AMI ships one; an Ubuntu client does not.
	RDSClientCACertPath = "/home/ubuntu/spinifex-ca.pem"

	// Each engine's assigned port, and the only one an instance of it publishes.
	PostgresEnginePort = 5432
	MariaDBEnginePort  = 3306

	rdsClientSSHTimeout   = 5 * time.Minute
	rdsClientSetupTimeout = 5 * time.Minute
)

// rdsClientUserData installs both engines' clients at first boot. Status and
// exit-code sentinels mirror the storagegrowth suite's protocol; the distinct
// exit codes separate "no apt egress" from "no such package", and one engine's
// client from the other's, which otherwise all surface as a missing binary much
// later.
var rdsClientUserData = fmt.Sprintf(`#!/bin/bash
set -u
trap 'echo $? > %[2]s' EXIT

echo running > %[1]s

export DEBIAN_FRONTEND=noninteractive
apt-get update -y || exit 10
apt-get install -y postgresql-client || exit 11
apt-get install -y mariadb-client || exit 12

echo done > %[1]s
`, rdsClientStatusFile, rdsClientExitCodeFile)

// RDSClientPlacement is where a client VM is put: the subnet it sits in and the
// security group it carries. Which subnet is the whole point for a caller
// proving an endpoint is reachable from somewhere other than its own.
type RDSClientPlacement struct {
	SubnetID string
	SGID     string
}

// RDSClientVM returns an SSH target for a postgres client VM in the customer's
// default VPC — the placement every test that only needs *a* client wants.
// Memoized per fixture: the boot, the install and the CA push happen once per
// process.
//
// The default VPC maps public IPs on launch and routes 0.0.0.0/0 at its IGW, so
// the same VM is reachable from the runner for SSH and has the apt egress the
// install needs.
func RDSClientVM(t *testing.T, c *AWSClient, fx *Fixture, env *Env) SSHTarget {
	t.Helper()
	vpc := EnsureDefaultVPC(t, fx)

	// The default SG admits only same-SG members, so the runner's SSH and the
	// client's connection to the DB endpoint both need opening explicitly. A
	// caller supplying its own placement brings its own groups instead. Both
	// engines' ports are opened here: the client VM is shared by every test that
	// needs a connection, whichever engine it is connecting to.
	EnsureDefaultSGOpen(t, c)
	AuthorizeTCPIngress(t, c, vpc.SGID, PostgresEnginePort)
	AuthorizeTCPIngress(t, c, vpc.SGID, MariaDBEnginePort)

	return RDSClientVMIn(t, c, fx, env, RDSClientPlacement{SubnetID: vpc.SubnetID, SGID: vpc.SGID})
}

// RDSClientVMIn is RDSClientVM in a placement the caller owns, for a topology
// the default VPC cannot express. The subnet must map public IPs on launch and
// reach an IGW, and the group must admit tcp/22 from the runner and let the
// client out to the endpoint: everything past that is the caller's own network.
//
// The memo is keyed through EnsureInstance, whose key already carries the subnet
// and group, so a second placement builds a second VM rather than returning the
// first.
func RDSClientVMIn(t *testing.T, c *AWSClient, fx *Fixture, env *Env, p RDSClientPlacement) SSHTarget {
	t.Helper()
	instType, arch := DiscoverNanoInstanceType(t, fx)
	ami := DiscoverUbuntuAMI(t, fx, arch)
	// The suite-level artifact root, not the calling test's subdirectory: the key
	// outlives the test that first asked for the VM, and ArtifactDir prunes a
	// passing test's directory — taking the PEM the next caller needs with it.
	keyName, keyPath := EnsureKeyPair(t, fx, env.ArtifactDir)

	instanceID := EnsureInstance(t, fx, InstanceSpec{
		AMIID:        ami,
		InstanceType: instType,
		KeyName:      keyName,
		SubnetID:     p.SubnetID,
		SGID:         p.SGID,
		UserData:     base64.StdEncoding.EncodeToString([]byte(rdsClientUserData)),
	})

	inst := WaitForInstanceState(t, c, instanceID, "running")
	host, port := InstancePublicSSHHost(t, inst)
	tgt := SSHTarget{User: rdsClientUser, Host: host, Port: port, KeyPath: keyPath}

	// Memoized apart from the instance so a second caller reuses the readied
	// guest rather than re-polling the install and re-pushing the CA.
	if _, err := fx.ensureOnce(t, "rds-client-vm:ready:"+instanceID, func() (string, func() error, error) {
		if !TryGuestSSHReady(host, port, rdsClientUser, keyPath, rdsClientSSHTimeout) {
			return "", nil, fmt.Errorf("client VM %s SSH %s:%d not ready after %s", instanceID, host, port, rdsClientSSHTimeout)
		}
		if err := waitForRDSClientTooling(t, tgt); err != nil {
			return "", nil, err
		}
		if err := pushClusterCA(tgt, env); err != nil {
			return "", nil, err
		}
		return instanceID, nil, nil
	}); err != nil {
		t.Fatalf("RDSClientVM: %v", err)
	}

	t.Logf("RDS client VM %s ready at %s@%s:%d", instanceID, rdsClientUser, host, port)
	return tgt
}

// waitForRDSClientTooling polls the boot-time install's sentinels until both
// clients are installed. A non-zero exit code is reported with the cloud-init
// log tail, since the interesting failure is always inside apt.
func waitForRDSClientTooling(t *testing.T, tgt SSHTarget) error {
	t.Helper()
	deadline := time.Now().Add(rdsClientSetupTimeout)
	var lastErr error
	status := "pending"
	for {
		// A transient SSH fault mid-install is normal; only its persistence past
		// the deadline is a failure, so it is carried, not swallowed — and neither
		// sentinel is read at all unless the read that produced it succeeded.
		readStatus, code, err := readRDSClientSentinels(tgt)
		switch {
		case err != nil:
			lastErr = err
		default:
			status = readStatus
			if code != "" && code != "0" {
				log, _ := GuestExec(tgt, "tail -40 /var/log/cloud-init-output.log 2>/dev/null || true")
				return fmt.Errorf("client VM database-client install exited %s (status=%q):\n%s", code, status, log)
			}
			if code == "0" && status == "done" {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("client VM database-client install still %q after %s (last ssh error: %v)",
				status, rdsClientSetupTimeout, lastErr)
		}
		time.Sleep(5 * time.Second)
	}
}

// Both sentinels, or neither. GuestExec returns the combined output, so ssh's
// own error text on a failed connection is indistinguishable from a sentinel's
// contents — reading one after a failure would take that text for an exit code.
func readRDSClientSentinels(tgt SSHTarget) (status, code string, err error) {
	status, err = GuestExec(tgt, "cat "+rdsClientStatusFile+" 2>/dev/null || echo pending")
	if err != nil {
		return "", "", err
	}
	code, err = GuestExec(tgt, "cat "+rdsClientExitCodeFile+" 2>/dev/null || true")
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(status), strings.TrimSpace(code), nil
}

// pushClusterCA copies the cluster CA into the guest so psql can be asked for
// verify-full. Base64 over the SSH command line rather than scp: a PEM is small
// and it needs no second transport.
func pushClusterCA(tgt SSHTarget, env *Env) error {
	caPath, err := ResolveCACert(env)
	if err != nil {
		return fmt.Errorf("locate cluster CA: %w", err)
	}
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("read cluster CA %s: %w", caPath, err)
	}
	cmd := fmt.Sprintf("echo %s | base64 -d > %s && chmod 0600 %s",
		base64.StdEncoding.EncodeToString(pem), RDSClientCACertPath, RDSClientCACertPath)
	if out, err := GuestExec(tgt, cmd); err != nil {
		return fmt.Errorf("push cluster CA into the client guest: %w\n%s", err, out)
	}
	return nil
}

// ----------------------------------------------------------------------------
// psql in the guest
// ----------------------------------------------------------------------------

// PSQLConn addresses one psql invocation from inside the client guest.
type PSQLConn struct {
	Host     string
	Port     int64
	User     string
	Password string
	DBName   string
	// SSLMode is handed to libpq verbatim. Empty leaves libpq's own default,
	// which is what an unpinned smoke connection wants; verify-full additionally
	// needs SSLRootCert.
	SSLMode string
	// SSLRootCert is an in-guest path to the trust root, normally
	// RDSClientCACertPath.
	SSLRootCert string
}

// PSQLConnFor builds a connection from an available DB instance's own reported
// endpoint, so a test cannot accidentally assert against an address the control
// plane never published.
func PSQLConnFor(t *testing.T, instance *rds.DBInstance, user, password, dbName string) PSQLConn {
	t.Helper()
	if instance == nil || instance.Endpoint == nil {
		t.Fatalf("PSQLConnFor: instance publishes no endpoint")
	}
	return PSQLConn{
		Host:     aws.StringValue(instance.Endpoint.Address),
		Port:     aws.Int64Value(instance.Endpoint.Port),
		User:     user,
		Password: password,
		DBName:   dbName,
	}
}

// One psql call, including connect. Generous enough for a cold connection to an
// engine that has just finished starting, tight enough to fail inside a subtest.
const psqlTimeout = 90 * time.Second

// PSQL runs sql through psql in the client guest and returns its output.
// ON_ERROR_STOP turns a failed statement into a non-zero exit rather than a
// warning buried in stdout.
//
// A failure fails the test. There is no skip path on the client leg: a green run
// that never opened a connection is the exact outcome this suite exists to
// prevent.
func PSQL(t *testing.T, tgt SSHTarget, conn PSQLConn, sql string) string {
	t.Helper()
	out, err := TryPSQL(tgt, conn, sql)
	if err != nil {
		t.Fatalf("psql %s@%s:%d/%s: %v\n%s", conn.User, conn.Host, conn.Port, conn.DBName, err, out)
	}
	return out
}

// TryPSQL is PSQL without the t.Fatal, for the cases where the connection is
// supposed to fail: a retired password, a security group that does not admit
// 5432, a certificate that must not verify.
func TryPSQL(tgt SSHTarget, conn PSQLConn, sql string) (string, error) {
	return GuestExecTimeout(tgt, psqlCommand(conn, sql), psqlTimeout)
}

// psqlCommand renders the guest-side command line. The password goes through the
// environment, never psql's argv, so it stays out of the guest's process list
// and out of any diagnostic that captures it.
func psqlCommand(conn PSQLConn, sql string) string {
	port := conn.Port
	if port == 0 {
		port = PostgresEnginePort
	}
	env := []string{
		"PGPASSWORD=" + ShellQuote(conn.Password),
		"PGCONNECT_TIMEOUT=30",
	}
	if conn.SSLMode != "" {
		env = append(env, "PGSSLMODE="+ShellQuote(conn.SSLMode))
	}
	if conn.SSLRootCert != "" {
		env = append(env, "PGSSLROOTCERT="+ShellQuote(conn.SSLRootCert))
	}
	args := []string{
		"psql", "--no-psqlrc", "--quiet", "--tuples-only", "--no-align",
		"--set", "ON_ERROR_STOP=1",
		"--host", ShellQuote(conn.Host),
		"--port", strconv.FormatInt(port, 10),
		"--username", ShellQuote(conn.User),
		"--dbname", ShellQuote(conn.DBName),
		"--command", ShellQuote(sql),
	}
	return strings.Join(env, " ") + " " + strings.Join(args, " ")
}

// ResolveInGuest returns the addresses host resolves to inside the guest.
//
// This is the only resolution that proves anything about the guest's DNS path
// The runner's own resolver may point straight at northstar, so a
// host-side lookup can pass on a cluster where no guest can resolve anything. An
// empty result means the name does not resolve, which is the assertion after a
// delete withdraws the record.
func ResolveInGuest(t *testing.T, tgt SSHTarget, host string) []string {
	t.Helper()
	// getent exits non-zero on NXDOMAIN, so the lookup's own failure is
	// swallowed in the guest: an error back here is an SSH fault, not an answer.
	out, err := GuestExec(tgt, "getent hosts "+ShellQuote(host)+" || true")
	if err != nil {
		t.Fatalf("ResolveInGuest %s: %v\n%s", host, err, out)
	}
	var addrs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && net.ParseIP(fields[0]) != nil {
			addrs = append(addrs, fields[0])
		}
	}
	return addrs
}
