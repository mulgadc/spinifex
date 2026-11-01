package workerkey_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// platform stands for the worker record behind the gateway: pending until a
// fingerprint is recorded, then exactly that fingerprint.
type platform struct {
	recorded  string
	recordErr error
	landed    bool
	records   int
}

func (p *platform) Claim(context.Context) (workerkey.Decision, error) {
	return workerkey.Decision{Pending: p.recorded == "", Fingerprint: p.recorded, HostNodes: 1}, nil
}

func (p *platform) RecordFingerprint(_ context.Context, fp string) error {
	p.records++
	if p.recordErr != nil {
		if p.landed {
			p.recorded = fp
		}
		return p.recordErr
	}
	p.recorded = fp
	return nil
}

func newStore(t *testing.T) workerkey.Store {
	t.Helper()
	return workerkey.Store{Dir: filepath.Join(t.TempDir(), "key")}
}

func noData() (bool, error) { return false, nil }

func TestAcquire_NewVolumeCreatesAndRecordsThenReloads(t *testing.T) {
	s, p := newStore(t), &platform{}
	key, err := s.Acquire(t.Context(), p, noData)
	require.NoError(t, err)
	assert.Len(t, key, workerkey.ReceiptKeyLen)
	assert.Equal(t, workerkey.Fingerprint(key), p.recorded)

	again, err := s.Acquire(t.Context(), p, func() (bool, error) { return true, nil })
	require.NoError(t, err)
	assert.Equal(t, key, again, "a recorded key is reloaded, store data or not")
	assert.Equal(t, 1, p.records)
}

// A reply lost after the write, or a crash before recording, resumes with the
// key already on disk rather than a new one.
func TestAcquire_LostRecordReplyResumesTheSameKey(t *testing.T) {
	for _, landed := range []bool{false, true} {
		s := newStore(t)
		p := &platform{recordErr: errors.New("connection reset"), landed: landed}
		_, err := s.Acquire(t.Context(), p, noData)
		require.Error(t, err)
		first, err := s.ReceiptKey()
		require.NoError(t, err)

		p.recordErr = nil
		key, err := s.Acquire(t.Context(), p, noData)
		require.NoError(t, err)
		assert.Equal(t, first, key)
		assert.Equal(t, workerkey.Fingerprint(first), p.recorded)
	}
}

// A pending decision on a disk that already holds store data creates nothing:
// the platform and the disk disagree about whether the volume is new.
func TestAcquire_PendingWithStoreDataCreatesNothing(t *testing.T) {
	s, p := newStore(t), &platform{}
	_, err := s.Acquire(t.Context(), p, func() (bool, error) { return true, nil })
	require.ErrorIs(t, err, workerkey.ErrDiskHasData)
	_, err = os.Stat(s.Dir)
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Zero(t, p.records)
}

// Once recorded, a missing, changed or damaged key refuses and is never
// regenerated: a new key would invalidate every handle already issued.
func TestAcquire_RecordedKeyIsNeverRegenerated(t *testing.T) {
	s, p := newStore(t), &platform{}
	key, err := s.Acquire(t.Context(), p, noData)
	require.NoError(t, err)
	path := filepath.Join(s.Dir, "receipt.key")

	require.NoError(t, os.Remove(path))
	_, err = s.Acquire(t.Context(), p, noData)
	require.ErrorIs(t, err, workerkey.ErrReceiptKeyMissing)
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist, "no replacement key is written")

	other := append([]byte(nil), key...)
	other[0] ^= 0xff
	require.NoError(t, os.WriteFile(path, other, 0o600))
	_, err = s.Acquire(t.Context(), p, noData)
	require.ErrorIs(t, err, workerkey.ErrKeyMismatch)

	require.NoError(t, os.WriteFile(path, key[:8], 0o600))
	_, err = s.Acquire(t.Context(), p, noData)
	require.Error(t, err)
	got, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, key[:8], got, "a damaged key is refused, not replaced")
	assert.Equal(t, 1, p.records)
}

func TestAcquire_ContradictoryDecisionRefuses(t *testing.T) {
	s := newStore(t)
	_, err := s.Acquire(t.Context(), contradictory{}, noData)
	require.Error(t, err)
	_, err = os.Stat(s.Dir)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

type contradictory struct{}

func (contradictory) Claim(context.Context) (workerkey.Decision, error) {
	return workerkey.Decision{Pending: true, Fingerprint: "ab"}, nil
}
func (contradictory) RecordFingerprint(context.Context, string) error { return nil }
