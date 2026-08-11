//go:build e2e

package harness

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
)

// The MariaDB half of the client leg. The guest and its trust root are the
// shared ones RDSClientVM readies — only the wire protocol differs, so only the
// command line does.

// MariaDBConn addresses one mariadb invocation from inside the client guest.
// The client is MariaDB's own rather than MySQL's: the server's authentication
// plugins and its TLS options are what MariaDB's client is written against.
type MariaDBConn struct {
	Host     string
	Port     int64
	User     string
	Password string
	DBName   string
	// SSLRootCert is an in-guest path to the trust root, normally
	// RDSClientCACertPath. Setting it is what turns TLS on: every option in the
	// client's --ssl-* family implies an encrypted connection. --ssl itself is
	// deliberately unused — the 11.x client deprecates it, and the deprecation
	// notice would land in the same stream as the query's own output.
	SSLRootCert string
	// VerifyServerCert checks the serving certificate against the host being
	// connected to, which is the other half of what verify-full means.
	VerifyServerCert bool
}

// MariaDBConnFor builds a connection from an available DB instance's own
// reported endpoint, so a test cannot accidentally assert against an address the
// control plane never published.
func MariaDBConnFor(t *testing.T, instance *rds.DBInstance, user, password, dbName string) MariaDBConn {
	t.Helper()
	if instance == nil || instance.Endpoint == nil {
		t.Fatalf("MariaDBConnFor: instance publishes no endpoint")
	}
	return MariaDBConn{
		Host:     aws.StringValue(instance.Endpoint.Address),
		Port:     aws.Int64Value(instance.Endpoint.Port),
		User:     user,
		Password: password,
		DBName:   dbName,
	}
}

// One mariadb call, including connect. Sized as psqlTimeout is: generous enough
// for a cold connection to an engine that has just finished starting, tight
// enough to fail inside a subtest.
const mariadbTimeout = 90 * time.Second

// MariaDB runs sql through the mariadb client in the client guest and returns
// its output.
//
// A failure fails the test, for the same reason PSQL's does: a green run that
// never opened a connection is the exact outcome this suite exists to prevent.
func MariaDB(t *testing.T, tgt SSHTarget, conn MariaDBConn, sql string) string {
	t.Helper()
	out, err := TryMariaDB(tgt, conn, sql)
	if err != nil {
		t.Fatalf("mariadb %s@%s:%d/%s: %v\n%s", conn.User, conn.Host, conn.Port, conn.DBName, err, out)
	}
	return out
}

// TryMariaDB is MariaDB without the t.Fatal, for the cases where the connection
// is supposed to fail: a retired password, a security group that does not admit
// 3306, a certificate that must not verify.
func TryMariaDB(tgt SSHTarget, conn MariaDBConn, sql string) (string, error) {
	return GuestExecTimeout(tgt, mariadbCommand(conn, sql), mariadbTimeout)
}

// mariadbCommand renders the guest-side command line. The password goes through
// MYSQL_PWD rather than the client's argv, so it stays out of the guest's
// process list and out of any diagnostic that captures it.
//
// --batch makes the output tab-separated and unpadded, and in that mode the
// client aborts on the first failed statement instead of carrying on, which is
// what turns a broken statement into a non-zero exit rather than a warning.
func mariadbCommand(conn MariaDBConn, sql string) string {
	port := conn.Port
	if port == 0 {
		port = MariaDBEnginePort
	}
	env := "MYSQL_PWD=" + shellQuote(conn.Password)
	args := []string{
		"mariadb", "--batch", "--skip-column-names", "--connect-timeout=30",
		"--host", shellQuote(conn.Host),
		"--port", strconv.FormatInt(port, 10),
		"--user", shellQuote(conn.User),
		"--database", shellQuote(conn.DBName),
	}
	if conn.SSLRootCert != "" {
		args = append(args, "--ssl-ca="+shellQuote(conn.SSLRootCert))
	}
	if conn.VerifyServerCert {
		args = append(args, "--ssl-verify-server-cert")
	}
	args = append(args, "--execute", shellQuote(sql))
	return env + " " + strings.Join(args, " ")
}
