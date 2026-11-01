// Package storekey keeps the SQS store guest's TLS server key on its data
// disk and records only the key's public pin with the gateway. A recorded pin
// with a missing or different key refuses: nothing regenerates it.
package storekey

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	sqsv1 "github.com/mulgadc/spinifex/contracts/sqs/v1"
)

const (
	keyFile    = "server-key.pem"
	tempPrefix = ".server-key-"
	tempSuffix = ".tmp"
	maxKeySize = 4 << 10
	certLife   = 100 * 365 * 24 * time.Hour
)

var (
	// ErrWrongStore is a record for a different data volume than this disk.
	ErrWrongStore = errors.New("store record names a different data volume")
	// ErrKeyMissing is a recorded pin with no key on the disk.
	ErrKeyMissing = errors.New("a server key pin is recorded but the disk has no server key")
	// ErrKeyChanged is a key on the disk that does not match the recorded pin.
	ErrKeyChanged = errors.New("the disk's server key does not match the recorded pin")
	// ErrUnsafe is a key file or directory that is not owner-only.
	ErrUnsafe = errors.New("server key file or directory is not owner-only")
	// ErrCorrupt is a key file that is not one P-256 private key.
	ErrCorrupt = errors.New("server key file is not a P-256 private key")
)

// Record is the store's own record as the gateway reports it.
type Record struct {
	ClaimID  string
	VolumeID string
	Pin      string
}

// Gateway reads and records the calling store's server key pin.
type Gateway interface {
	DescribeServerKey(ctx context.Context) (Record, error)
	RecordServerKey(ctx context.Context, claimID, volumeID, pin string) error
}

// Config names the key directory on the mounted data disk, this disk's
// volume, the listen address the certificate names and the retry bound.
type Config struct {
	Dir      string
	VolumeID string
	Address  string
	CertPath string
	Gateway  Gateway
	Attempts int
	Wait     time.Duration
	Log      func(string)
}

// Ensure loads or creates the server key, records its pin unless the same pin
// is already recorded, and writes a self-signed certificate for it to
// CertPath. It returns the key file's path; the key never leaves the disk.
func Ensure(ctx context.Context, cfg Config) (string, error) {
	unlock, err := prepareDir(cfg.Dir)
	if err != nil {
		return "", err
	}
	defer unlock()
	var rec Record
	if err := retry(ctx, cfg, "describe store server key", func() (err error) {
		rec, err = cfg.Gateway.DescribeServerKey(ctx)
		return err
	}); err != nil {
		return "", err
	}
	if rec.VolumeID != cfg.VolumeID {
		return "", ErrWrongStore
	}
	path := filepath.Join(cfg.Dir, keyFile)
	key, err := load(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && rec.Pin != "":
		return "", ErrKeyMissing
	case errors.Is(err, fs.ErrNotExist):
		if key, err = create(cfg.Dir, path); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	}
	cert, err := selfSigned(key, cfg.Address)
	if err != nil {
		return "", err
	}
	pin := sqsv1.Pin(cert)
	switch {
	case rec.Pin == pin:
	case rec.Pin != "":
		return "", ErrKeyChanged
	default:
		if err := retry(ctx, cfg, "record store server key", func() error {
			return cfg.Gateway.RecordServerKey(ctx, rec.ClaimID, rec.VolumeID, pin)
		}); err != nil {
			return "", err
		}
		cfg.Log("server key pin recorded")
	}
	if err := writeCert(cfg.CertPath, cert.Raw); err != nil {
		return "", err
	}
	return path, nil
}

// retry repeats op while the gateway reports a retryable refusal or a launch
// not yet bound to this instance; any other refusal is final.
func retry(ctx context.Context, cfg Config, what string, op func() error) error {
	var err error
	for i := range max(cfg.Attempts, 1) {
		if err = op(); err == nil {
			return nil
		}
		if !again(err) {
			return fmt.Errorf("%s: %w", what, err)
		}
		if i == 0 {
			cfg.Log(what + ": not available yet, retrying")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.Wait):
		}
	}
	return fmt.Errorf("%s: gave up: %w", what, err)
}

// again reports a retryable refusal or a launch not yet bound.
func again(err error) bool {
	var r interface{ Retryable() bool }
	var b interface{ NotBound() bool }
	return (errors.As(err, &r) && r.Retryable()) || (errors.As(err, &b) && b.NotBound())
}

// prepareDir creates only the key directory, refuses one that is not
// owner-only, locks it and removes temporary files a crash left behind.
func prepareDir(dir string) (func(), error) {
	if fi, err := os.Stat(filepath.Dir(dir)); err != nil || !fi.IsDir() {
		return nil, errors.New("server key directory parent is missing")
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("create server key directory: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("stat server key directory: %w", err)
	}
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return nil, ErrUnsafe
	}
	d, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open server key directory: %w", err)
	}
	if err := syscall.Flock(int(d.Fd()), syscall.LOCK_EX); err != nil {
		d.Close()
		return nil, fmt.Errorf("lock server key directory: %w", err)
	}
	stale, _ := filepath.Glob(filepath.Join(dir, tempPrefix+"*"+tempSuffix))
	for _, f := range stale {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			d.Close()
			return nil, fmt.Errorf("remove stale server key file: %w", err)
		}
	}
	return func() { d.Close() }, nil
}

// load reads an existing key. A file that is present but unusable refuses
// rather than being replaced.
func load(path string) (*ecdsa.PrivateKey, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return nil, ErrUnsafe
	}
	if fi.Size() > maxKeySize {
		return nil, ErrCorrupt
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read server key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, ErrCorrupt
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, ErrCorrupt
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, ErrCorrupt
	}
	return ec, nil
}

// create writes a new key to a synced temporary file and links it into place
// without replacing anything, then makes the names durable.
func create(dir, path string) (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate server key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode server key: %w", err)
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*"+tempSuffix)
	if err != nil {
		return nil, fmt.Errorf("create server key file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := writeSynced(tmp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		return nil, err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return load(path)
		}
		return nil, fmt.Errorf("install server key file: %w", err)
	}
	for _, d := range []string{dir, filepath.Dir(dir)} {
		if err := syncDir(d); err != nil {
			return nil, err
		}
	}
	return key, nil
}

// selfSigned issues a certificate for key. The worker trusts the key's pin,
// not the certificate, so it is reissued each boot.
func selfSigned(key *ecdsa.PrivateKey, addr string) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("server certificate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "sqs-store"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(certLife),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(addr); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("issue server certificate: %w", err)
	}
	return x509.ParseCertificate(der)
}

func writeCert(path string, der []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".server-cert-*")
	if err != nil {
		return fmt.Errorf("create server certificate: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := writeSynced(tmp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func writeSynced(f *os.File, data []byte) error {
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("restrict server key file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write server key file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync server key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close server key file: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open server key directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync server key directory: %w", err)
	}
	return nil
}
