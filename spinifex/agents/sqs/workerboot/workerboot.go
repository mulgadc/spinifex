// Package workerboot prepares the shared SQS worker guest: it selects exactly
// the expected data disk by serial, checks and mounts it, proves that disk is
// what is mounted, and only then loads or creates the worker's store key on it
// under the current first-store decision the gateway returns.
package workerboot

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
	"github.com/nats-io/nkeys"
)

// Guest contract, read from fw_cfg under opt/spinifex/sqs-worker/. None of it
// is a decision: whether a store key may be created is asked of the gateway.
//
//	data-serial   virtio-blk serial of the worker's own ext4 data disk
//	region        the Region the worker signs its gateway calls for
//	gateway-url   https URL of the gateway on this node's mgmt bridge
const (
	keyDirName = "sqs-worker-key"
	maxFwCfg   = 128
)

var regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

// NewClaimer builds the gateway client the first-store decision comes from.
type NewClaimer func(region, gatewayURL string) (workerkey.Claimer, error)

// Disk checks and mounts the worker data disk. The guest never formats.
type Disk interface {
	Check(dev string) error
	Mount(dev, dir string) error
}

// Config holds every path the boot step reads or writes, so tests can run it
// against a fake fw_cfg, sysfs and mount table.
type Config struct {
	FwCfgDir  string
	SysBlock  string
	DevDir    string
	MountInfo string
	DataDir   string
	RunDir    string
	Disk      Disk
	// IsBlockDevice reports whether a /dev node is a block device.
	IsBlockDevice func(path string) bool
	NewClaimer    NewClaimer
	// Attempts bounds the decision calls, Wait apart. Only a lost reply, a
	// retryable refusal or a pending authorization is asked again.
	Attempts int
	Wait     time.Duration
	Log      func(msg string)
}

type contract struct {
	serial, region, gatewayURL string
}

// Boot runs the boot step and returns the worker's public key. The key is
// loaded or created only under the gateway's current decision; any missing,
// malformed or ambiguous input or answer refuses.
func Boot(ctx context.Context, cfg Config) (string, error) {
	c, err := readContract(cfg.FwCfgDir)
	if err != nil {
		return "", err
	}
	if err := mountWorkerDisk(cfg, c.serial); err != nil {
		return "", err
	}

	if cfg.NewClaimer == nil {
		return "", errors.New("no gateway client is configured")
	}
	claimer, err := cfg.NewClaimer(c.region, c.gatewayURL)
	if err != nil {
		return "", fmt.Errorf("build gateway client: %w", err)
	}
	kp, err := acquire(ctx, cfg, workerkey.Store{Dir: filepath.Join(cfg.DataDir, keyDirName)}, claimer)
	if err != nil {
		return "", err
	}
	defer kp.Wipe()
	pub, err := kp.PublicKey()
	if err != nil {
		return "", fmt.Errorf("derive worker public key: %w", err)
	}
	if err := writePublicKey(cfg.RunDir, pub); err != nil {
		return "", err
	}
	cfg.Log("store key ready: public=" + pub)
	return pub, nil
}

// mountWorkerDisk selects the disk with serial, checks and mounts it at
// DataDir unless it is already there, and proves from the mount table that
// exactly that disk is what is mounted.
func mountWorkerDisk(cfg Config, serial string) error {
	name, err := findDisk(cfg, serial)
	if err != nil {
		return err
	}
	dev := filepath.Join(cfg.DevDir, name)
	majMin, err := readTrimmed(filepath.Join(cfg.SysBlock, name, "dev"))
	if err != nil {
		return fmt.Errorf("read device number of %s: %w", name, err)
	}

	mounted, err := mountedDevice(cfg.MountInfo, cfg.DataDir)
	if err != nil {
		return err
	}
	switch mounted {
	case majMin:
		cfg.Log(fmt.Sprintf("%s already mounted at %s", dev, cfg.DataDir))
	case "":
		if err := cfg.Disk.Check(dev); err != nil {
			return fmt.Errorf("check %s: %w", dev, err)
		}
		if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
			return fmt.Errorf("create mountpoint %s: %w", cfg.DataDir, err)
		}
		if err := cfg.Disk.Mount(dev, cfg.DataDir); err != nil {
			return fmt.Errorf("mount %s at %s: %w", dev, cfg.DataDir, err)
		}
	default:
		return fmt.Errorf("%s holds device %s, not the worker disk %s", cfg.DataDir, mounted, majMin)
	}

	// Proven from the mount table, not inferred from the mount call: an empty
	// mountpoint directory must never receive a key.
	mounted, err = mountedDevice(cfg.MountInfo, cfg.DataDir)
	if err != nil {
		return err
	}
	if mounted != majMin {
		return fmt.Errorf("worker disk %s (%s) is not mounted at %s", dev, majMin, cfg.DataDir)
	}
	return nil
}

func readContract(dir string) (contract, error) {
	var c contract
	var err error
	if c.serial, err = readFwCfg(dir, "data-serial"); err != nil {
		return c, err
	}
	if !validSerial(c.serial) {
		return c, errors.New("data-serial is not a volume serial")
	}
	if c.region, err = readFwCfg(dir, "region"); err != nil {
		return c, err
	}
	if !regionPattern.MatchString(c.region) {
		return c, errors.New("region is not a Region name")
	}
	if c.gatewayURL, err = readFwCfg(dir, "gateway-url"); err != nil {
		return c, err
	}
	if u, err := url.Parse(c.gatewayURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return c, errors.New("gateway-url is not an https URL")
	}
	return c, nil
}

// acquire runs the decision and key step, repeating it only on an error that
// says it is safe to repeat, including a denial a not-yet-bound worker gets.
// A denial creates nothing, and each repeat re-asks the gateway.
func acquire(ctx context.Context, cfg Config, store workerkey.Store, c workerkey.Claimer) (kp nkeys.KeyPair, err error) {
	attempts := max(cfg.Attempts, 1)
	for i := range attempts {
		kp, err = store.Acquire(ctx, c)
		if err == nil || i == attempts-1 {
			return kp, err
		}
		switch {
		case pending(err):
			cfg.Log(fmt.Sprintf("worker not yet authorized, asking again: %v", err))
		case retryable(err):
			cfg.Log(fmt.Sprintf("first-store decision not settled, retrying: %v", err))
		default:
			return kp, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(cfg.Wait):
		}
	}
	return nil, err
}

func pending(err error) bool {
	p, ok := errors.AsType[interface {
		error
		Pending() bool
	}](err)
	return ok && p.Pending()
}

func retryable(err error) bool {
	r, ok := errors.AsType[interface {
		error
		Retryable() bool
	}](err)
	return ok && r.Retryable()
}

// readFwCfg returns one fw_cfg value: a single token with at most one trailing
// newline. The value is never echoed in an error.
func readFwCfg(dir, name string) (string, error) {
	path := filepath.Join(dir, name, "raw")
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("fw_cfg %s not supplied: %w", name, fs.ErrNotExist)
		}
		return "", fmt.Errorf("fw_cfg %s unreadable", name)
	}
	if fi.Size() > maxFwCfg {
		return "", fmt.Errorf("fw_cfg %s is oversized", name)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("fw_cfg %s unreadable", name)
	}
	v := strings.TrimSuffix(string(b), "\n")
	if v == "" || strings.ContainsAny(v, " \t\r\n\v\f") {
		return "", fmt.Errorf("fw_cfg %s must be exactly one value", name)
	}
	return v, nil
}

// A volume serial is the volume ID with its dash removed: vol + 17 hex.
func validSerial(s string) bool {
	if len(s) != 20 || !strings.HasPrefix(s, "vol") {
		return false
	}
	for _, r := range s[3:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// findDisk returns the block device name whose serial matches exactly once.
func findDisk(cfg Config, serial string) (string, error) {
	entries, err := os.ReadDir(cfg.SysBlock)
	if err != nil {
		return "", fmt.Errorf("list block devices: %w", err)
	}
	var found []string
	for _, e := range entries {
		s, err := readTrimmed(filepath.Join(cfg.SysBlock, e.Name(), "serial"))
		if err == nil && s == serial {
			found = append(found, e.Name())
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("expected one disk with serial %s, found %d", serial, len(found))
	}
	if !cfg.IsBlockDevice(filepath.Join(cfg.DevDir, found[0])) {
		return "", fmt.Errorf("disk %s has no block device node", found[0])
	}
	return found[0], nil
}

// mountedDevice returns the major:minor of the topmost ext4 mount of a whole
// filesystem at dir, or "" when nothing is mounted there.
func mountedDevice(mountInfo, dir string) (string, error) {
	f, err := os.Open(mountInfo)
	if err != nil {
		return "", fmt.Errorf("read mount table: %w", err)
	}
	defer f.Close()
	dir = filepath.Clean(dir)
	var majMin string
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, fld := range fields {
			if fld == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || sep+1 >= len(fields) {
			continue
		}
		if unescapeMountPath(fields[4]) != dir {
			continue
		}
		found = true
		majMin = ""
		if fields[3] == "/" && fields[sep+1] == "ext4" {
			majMin = fields[2]
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read mount table: %w", err)
	}
	if found && majMin == "" {
		return "", fmt.Errorf("%s is mounted, but not as a whole ext4 filesystem", dir)
	}
	return majMin, nil
}

// unescapeMountPath decodes the octal escapes mountinfo uses for space, tab,
// newline and backslash.
func unescapeMountPath(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// writePublicKey publishes the public key for later reporting; it is not secret.
func writePublicKey(runDir, pub string) error {
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", runDir, err)
	}
	tmp, err := os.CreateTemp(runDir, ".public-key-*")
	if err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(pub + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("write public key: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("write public key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(runDir, "public-key")); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	return nil
}
