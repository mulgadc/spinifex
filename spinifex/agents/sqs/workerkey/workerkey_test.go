package workerkey

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nkeys"
)

func newStore(t *testing.T) Store {
	t.Helper()
	return Store{Dir: filepath.Join(t.TempDir(), "keys")}
}

func publicKey(t *testing.T, kp nkeys.KeyPair) string {
	t.Helper()
	pub, err := kp.PublicKey()
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	return pub
}

func otherUserKey(t *testing.T) string {
	t.Helper()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return publicKey(t, kp)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func assertNoSeed(t *testing.T, err error, seed []byte) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), string(seed)) {
		t.Fatalf("error exposes the seed: %v", err)
	}
}

func TestEnsure_FirstBootPersistsAndRestartReusesKey(t *testing.T) {
	s := newStore(t)
	kp, err := s.ForNewStore()
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	pub := publicKey(t, kp)

	dirInfo, err := os.Stat(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode = %o, want 700", got)
	}
	fileInfo, err := os.Stat(s.path())
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("key file mode = %o, want 600", got)
	}
	onDisk, err := nkeys.FromSeed(readFile(t, s.path()))
	if err != nil {
		t.Fatalf("persisted file is not a seed: %v", err)
	}
	if publicKey(t, onDisk) != pub {
		t.Fatal("persisted seed does not match the returned key")
	}

	// A restart is a fresh Store over the same disk; neither path may
	// generate a replacement.
	again, err := Store{Dir: s.Dir}.ForExistingStore(pub)
	if err != nil {
		t.Fatalf("restart for the existing store: %v", err)
	}
	if publicKey(t, again) != pub {
		t.Fatal("restart for the existing store changed the key")
	}
	again, err = Store{Dir: s.Dir}.ForNewStore()
	if err != nil {
		t.Fatalf("repeated first start: %v", err)
	}
	if publicKey(t, again) != pub {
		t.Fatal("repeated first start replaced the key")
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("key directory holds %d entries, want only the key", len(entries))
	}
}

func TestEnsure_ExistingStoreWithMissingKeyFailsClosed(t *testing.T) {
	s := newStore(t)
	_, err := s.ForExistingStore(otherUserKey(t))
	if !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("err = %v, want ErrKeyMissing", err)
	}
	if _, err := os.Lstat(s.path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a key was generated for an existing store: %v", err)
	}
}

func TestEnsure_MismatchedKeyFailsClosedAndKeepsFile(t *testing.T) {
	s := newStore(t)
	if _, err := s.ForNewStore(); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, s.path())

	_, err := Store{Dir: s.Dir}.ForExistingStore(otherUserKey(t))
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("err = %v, want ErrKeyMismatch", err)
	}
	assertNoSeed(t, err, before)
	if string(readFile(t, s.path())) != string(before) {
		t.Fatal("mismatch replaced the key file")
	}
}

func TestEnsure_UnusableKeyFileIsNeverReplaced(t *testing.T) {
	seedKP, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	userSeed, err := seedKP.Seed()
	if err != nil {
		t.Fatal(err)
	}
	accountKP, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	accountSeed, err := accountKP.Seed()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		content []byte
		mode    os.FileMode
		want    error
	}{
		{"empty", nil, 0o600, ErrCorrupt},
		{"garbage", []byte("not a seed"), 0o600, ErrCorrupt},
		{"account seed", accountSeed, 0o600, ErrCorrupt},
		{"oversized", make([]byte, maxKeyFileSize+1), 0o600, ErrCorrupt},
		{"group readable", userSeed, 0o640, ErrUnsafe},
		{"world readable", userSeed, 0o604, ErrUnsafe},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			if err := os.MkdirAll(s.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.path(), tc.content, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(s.path(), tc.mode); err != nil {
				t.Fatal(err)
			}
			_, err := s.ForNewStore()
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			assertNoSeed(t, err, userSeed)
			assertNoSeed(t, err, accountSeed)
			if string(readFile(t, s.path())) != string(tc.content) {
				t.Fatal("an unusable key file was replaced")
			}
		})
	}
}

func TestEnsure_RefusesUnsafeDirectoryAndSymlink(t *testing.T) {
	t.Run("open directory", func(t *testing.T) {
		s := newStore(t)
		if err := os.MkdirAll(s.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(s.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ForNewStore(); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("err = %v, want ErrUnsafe", err)
		}
		if _, err := os.Lstat(s.path()); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a key was written into an open directory")
		}
	})
	t.Run("symlinked key", func(t *testing.T) {
		s := newStore(t)
		target := newStore(t)
		if _, err := target.ForNewStore(); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(s.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target.path(), s.path()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ForNewStore(); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("err = %v, want ErrUnsafe", err)
		}
	})
}

func TestEnsure_CrashLeftoverTempIsNotAKey(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(s.Dir, tempPrefix+"123"+tempSuffix)
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A crash before the link means no store was authorized for it.
	if _, err := s.ForExistingStore(otherUserKey(t)); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("err = %v, want ErrKeyMissing", err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale temporary key file was kept")
	}
}

func TestEnsure_ConcurrentFirstBootAgreesOnOneKey(t *testing.T) {
	const rounds, n = 20, 16
	for range rounds {
		s := newStore(t)
		pubs := make([]string, n)
		errs := make([]error, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				<-start
				kp, err := Store{Dir: s.Dir}.ForNewStore()
				if err != nil {
					errs[i] = err
					return
				}
				pubs[i], errs[i] = kp.PublicKey()
			})
		}
		close(start)
		wg.Wait()
		onDisk, err := nkeys.FromSeed(readFile(t, s.path()))
		if err != nil {
			t.Fatal(err)
		}
		want := publicKey(t, onDisk)
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("call %d: %v", i, errs[i])
			}
			if pubs[i] != want {
				t.Fatalf("call %d returned a key that is not the persisted one", i)
			}
		}
	}
}

func TestForExistingStore_RejectsInvalidAuthorizedKey(t *testing.T) {
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := kp.Seed()
	if err != nil {
		t.Fatal(err)
	}
	// An empty key is what an unreadable store record would yield; it must not
	// fall through to first-start generation.
	for _, authorized := range []string{"", string(seed)} {
		s := newStore(t)
		_, err := s.ForExistingStore(authorized)
		if err == nil {
			t.Fatalf("authorized key %q was accepted", authorized[:min(len(authorized), 2)])
		}
		assertNoSeed(t, err, seed)
		if _, err := os.Lstat(s.Dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("an invalid authorized key still touched the disk")
		}
	}
}

func TestForNewStore_RefusesMissingParent(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "missing-parent", "keys")}
	if _, err := s.ForNewStore(); err == nil {
		t.Fatal("a key directory was created without its parent")
	}
	if _, err := os.Lstat(filepath.Dir(s.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the missing parent was created")
	}
}

// hookSyncs records every directory sync and fails call number failAt
// (counted from one); zero fails none.
func hookSyncs(t *testing.T, failAt int) *[]string {
	t.Helper()
	var synced []string
	orig := syncDirFn
	t.Cleanup(func() { syncDirFn = orig })
	syncDirFn = func(dir string) error {
		synced = append(synced, dir)
		if len(synced) == failAt {
			return errors.New("injected directory sync failure")
		}
		return orig(dir)
	}
	return &synced
}

func TestEnsure_RetryAfterFailedDirectorySyncKeepsKeyAndSyncs(t *testing.T) {
	retries := map[string]func(s Store, pub string) (nkeys.KeyPair, error){
		"first start": func(s Store, _ string) (nkeys.KeyPair, error) { return s.ForNewStore() },
		"existing store": func(s Store, pub string) (nkeys.KeyPair, error) {
			return s.ForExistingStore(pub)
		},
	}
	for name, retry := range retries {
		for failAt := 1; failAt <= 2; failAt++ {
			t.Run(fmt.Sprintf("%s after sync %d failed", name, failAt), func(t *testing.T) {
				s := newStore(t)
				hookSyncs(t, failAt)
				if _, err := s.ForNewStore(); err == nil {
					t.Fatal("a key was returned although a directory sync failed")
				}
				first := readFile(t, s.path())
				onDisk, err := nkeys.FromSeed(first)
				if err != nil {
					t.Fatal(err)
				}
				pub := publicKey(t, onDisk)

				synced := hookSyncs(t, 0)
				kp, err := retry(Store{Dir: s.Dir}, pub)
				if err != nil {
					t.Fatalf("retry: %v", err)
				}
				if string(readFile(t, s.path())) != string(first) {
					t.Fatal("retry replaced the key")
				}
				if publicKey(t, kp) != pub {
					t.Fatal("retry returned a key other than the persisted one")
				}
				want := []string{s.Dir, filepath.Dir(s.Dir)}
				if len(*synced) != 2 || (*synced)[0] != want[0] || (*synced)[1] != want[1] {
					t.Fatalf("retry synced %v, want %v", *synced, want)
				}
			})
		}
	}
}

// Another holder of the directory lock, such as a second worker process on
// the same disk, must finish before this one reads, cleans or creates a key.
func TestEnsure_WaitsForDirectoryLock(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	holder, err := os.Open(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := Store{Dir: s.Dir}.ForNewStore()
		done <- err
	}()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("Ensure returned while another holder had the lock: %v", err)
		default:
		}
		if _, err := os.Lstat(s.path()); err == nil {
			t.Fatal("a key was created while another holder had the lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Ensure after unlock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ensure did not proceed after the lock was released")
	}
}
