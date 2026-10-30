// Package workerboot prepares the shared SQS worker guest: it selects exactly
// the expected data disk by serial, checks and mounts it, proves that disk is
// what is mounted, and only then loads or creates the worker's store key on it.
package workerboot

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
	"github.com/nats-io/nkeys"
)

// Guest contract, read from fw_cfg under opt/spinifex/sqs-worker/:
//
//	data-serial   virtio-blk serial of the worker's own ext4 data disk
//	store-status  "existing"; "new" is refused (see readContract)
//	store-nkey    the store's authorized user public NKey
const (
	StatusExisting = "existing"
	statusNew      = "new"

	keyDirName = "sqs-worker-key"
	maxFwCfg   = 128
)

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
	Log           func(msg string)
}

type contract struct {
	serial, status, storeKey string
}

// Boot runs the boot step and returns the worker's public key. It only loads
// an existing key the store authorizes; any missing, malformed or ambiguous
// input refuses before a key is read.
func Boot(cfg Config) (string, error) {
	c, err := readContract(cfg.FwCfgDir)
	if err != nil {
		return "", err
	}
	name, err := findDisk(cfg, c.serial)
	if err != nil {
		return "", err
	}
	dev := filepath.Join(cfg.DevDir, name)
	majMin, err := readTrimmed(filepath.Join(cfg.SysBlock, name, "dev"))
	if err != nil {
		return "", fmt.Errorf("read device number of %s: %w", name, err)
	}

	mounted, err := mountedDevice(cfg.MountInfo, cfg.DataDir)
	if err != nil {
		return "", err
	}
	switch mounted {
	case majMin:
		cfg.Log(fmt.Sprintf("%s already mounted at %s", dev, cfg.DataDir))
	case "":
		if err := cfg.Disk.Check(dev); err != nil {
			return "", fmt.Errorf("check %s: %w", dev, err)
		}
		if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
			return "", fmt.Errorf("create mountpoint %s: %w", cfg.DataDir, err)
		}
		if err := cfg.Disk.Mount(dev, cfg.DataDir); err != nil {
			return "", fmt.Errorf("mount %s at %s: %w", dev, cfg.DataDir, err)
		}
	default:
		return "", fmt.Errorf("%s holds device %s, not the worker disk %s", cfg.DataDir, mounted, majMin)
	}

	// Proven from the mount table, not inferred from the mount call: an empty
	// mountpoint directory must never receive a key.
	mounted, err = mountedDevice(cfg.MountInfo, cfg.DataDir)
	if err != nil {
		return "", err
	}
	if mounted != majMin {
		return "", fmt.Errorf("worker disk %s (%s) is not mounted at %s", dev, majMin, cfg.DataDir)
	}

	kp, err := workerkey.Store{Dir: filepath.Join(cfg.DataDir, keyDirName)}.ForExistingStore(c.storeKey)
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

func readContract(dir string) (contract, error) {
	var c contract
	var err error
	if c.serial, err = readFwCfg(dir, "data-serial"); err != nil {
		return c, err
	}
	if !validSerial(c.serial) {
		return c, errors.New("data-serial is not a volume serial")
	}
	if c.status, err = readFwCfg(dir, "store-status"); err != nil {
		return c, err
	}
	switch c.status {
	case StatusExisting:
	case statusNew:
		// A static fw_cfg value replays across resets, so it cannot prove no
		// store exists; key creation waits for a current authoritative decision.
		return c, errors.New(`store-status "new" is refused: creating a store key needs a current authoritative first-store decision`)
	default:
		return c, errors.New(`store-status must be "existing"`)
	}
	key, err := readFwCfg(dir, "store-nkey")
	if err != nil {
		return c, err
	}
	if !nkeys.IsValidPublicUserKey(key) {
		return c, errors.New("store-nkey is not a user public NKey")
	}
	c.storeKey = key
	return c, nil
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
