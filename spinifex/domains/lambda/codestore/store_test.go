package codestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testRef = Ref{AccountID: "000000000001", FunctionID: "fn-0123", RevisionID: "rev-1"}

func copyKey(sum string) string { return "000000000001/fn-0123/rev-1/" + sum + ".zip" }

func TestStore_PutGetDelete(t *testing.T) {
	mem := objectstore.NewMemoryObjectStore()
	s := newStore(testRegion, mem)
	code := []byte("PK\x03\x04 zip bytes")

	sum, err := s.Put(t.Context(), testRef, code)
	require.NoError(t, err)
	want := sha256.Sum256(code)
	assert.Equal(t, hex.EncodeToString(want[:]), sum)

	_, err = mem.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String("lambda-code-ap-southeast-2"), Key: aws.String(copyKey(sum))})
	require.NoError(t, err, "copy is keyed by account, function, revision and digest")

	got, err := s.Get(t.Context(), testRef, sum)
	require.NoError(t, err)
	assert.Equal(t, code, got)

	require.NoError(t, s.Delete(t.Context(), testRef, sum))
	_, err = s.Get(t.Context(), testRef, sum)
	require.ErrorIs(t, err, ErrCodeNotFound)
	require.NoError(t, s.Delete(t.Context(), testRef, sum), "deleting an absent copy succeeds")
}

// A retry or a racing write with different bytes for the same revision lands on
// its own digest key, so the recorded copy is never replaced.
func TestStore_DifferentBytesNeverReplaceRecordedCopy(t *testing.T) {
	s := newStore(testRegion, objectstore.NewMemoryObjectStore())
	recorded, err := s.Put(t.Context(), testRef, []byte("first"))
	require.NoError(t, err)

	var wg sync.WaitGroup
	others := make([]string, 8)
	for i := range others {
		wg.Go(func() {
			sum, err := s.Put(t.Context(), testRef, []byte(fmt.Sprintf("other-%d", i)))
			assert.NoError(t, err)
			others[i] = sum
		})
	}
	wg.Wait()

	got, err := s.Get(t.Context(), testRef, recorded)
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), got)
	for i, sum := range others {
		got, err := s.Get(t.Context(), testRef, sum)
		require.NoError(t, err)
		assert.Equal(t, []byte(fmt.Sprintf("other-%d", i)), got)
	}
}

// DeleteRevision clears every copy of one revision, including a failed
// attempt's, and leaves other revisions alone.
func TestStore_DeleteRevisionRemovesAllCopiesOfThatRevisionOnly(t *testing.T) {
	s := newStore(testRegion, objectstore.NewMemoryObjectStore())
	a, err := s.Put(t.Context(), testRef, []byte("kept attempt"))
	require.NoError(t, err)
	b, err := s.Put(t.Context(), testRef, []byte("failed attempt"))
	require.NoError(t, err)
	sibling := Ref{AccountID: testRef.AccountID, FunctionID: testRef.FunctionID, RevisionID: "rev-10"}
	c, err := s.Put(t.Context(), sibling, []byte("next revision"))
	require.NoError(t, err)

	require.NoError(t, s.DeleteRevision(t.Context(), testRef))

	for _, sum := range []string{a, b} {
		_, err := s.Get(t.Context(), testRef, sum)
		require.ErrorIs(t, err, ErrCodeNotFound)
	}
	_, err = s.Get(t.Context(), sibling, c)
	require.NoError(t, err, "rev-10 shares the rev-1 string prefix but not its key prefix")
}

func TestStore_GetRefusesDigestMismatch(t *testing.T) {
	mem := objectstore.NewMemoryObjectStore()
	s := newStore(testRegion, mem)
	sum, err := s.Put(t.Context(), testRef, []byte("original"))
	require.NoError(t, err)
	_, err = mem.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(BucketName(testRegion)), Key: aws.String(copyKey(sum)),
		Body: bytes.NewReader([]byte("replaced"))})
	require.NoError(t, err)

	_, err = s.Get(t.Context(), testRef, sum)
	require.ErrorIs(t, err, ErrDigestMismatch)
}

func TestStore_RejectsUnsafeRefAndOversize(t *testing.T) {
	s := newStore(testRegion, objectstore.NewMemoryObjectStore())
	for _, ref := range []Ref{
		{AccountID: "", FunctionID: "f", RevisionID: "r"},
		{AccountID: "a", FunctionID: "..", RevisionID: "r"},
		{AccountID: "a", FunctionID: "f/../../other", RevisionID: "r"},
	} {
		_, err := s.Put(t.Context(), ref, []byte("x"))
		require.ErrorIs(t, err, ErrInvalidRef, "%+v", ref)
	}
	_, err := s.Put(t.Context(), testRef, make([]byte, MaxCodeBytes+1))
	require.ErrorIs(t, err, ErrCodeTooLarge)
	_, err = s.Get(t.Context(), testRef, "../../other")
	require.ErrorIs(t, err, ErrInvalidRef, "a digest is never used as a raw key segment")
}

func TestOpen_FailsWhenRoleNotAssumable(t *testing.T) {
	_, err := Open(OpenConfig{Region: testRegion, Endpoint: "https://127.0.0.1:1", SigningRegion: testRegion,
		Assume: func() (*sts.AssumeRoleOutput, error) { return nil, errors.New("AccessDenied") }})
	require.Error(t, err)
}

func TestManager_InitialisesOnFirstUseWithBackoff(t *testing.T) {
	attempts := 0
	m := NewManager(func(context.Context) error {
		attempts++
		if attempts < 3 {
			return ErrNotOwned
		}
		return nil
	}, func() (*Store, error) { return newStore(testRegion, objectstore.NewMemoryObjectStore()), nil })
	now := time.Unix(1_000_000, 0)
	m.now = func() time.Time { return now }
	assert.Zero(t, attempts, "construction does no provisioning")

	_, err := m.Store(t.Context())
	require.ErrorIs(t, err, ErrNotReady)
	require.ErrorIs(t, err, ErrNotOwned)
	_, err = m.Store(t.Context())
	require.ErrorIs(t, err, ErrNotReady)
	assert.Equal(t, 1, attempts, "a call inside the backoff window does not retry")

	now = now.Add(time.Second)
	_, err = m.Store(t.Context())
	require.ErrorIs(t, err, ErrNotReady)
	now = now.Add(time.Second)
	_, err = m.Store(t.Context())
	require.ErrorIs(t, err, ErrNotReady, "backoff doubled to 2s")
	now = now.Add(time.Second)
	first, err := m.Store(t.Context())
	require.NoError(t, err)
	again, err := m.Store(t.Context())
	require.NoError(t, err)
	assert.Same(t, first, again)
	assert.Equal(t, 3, attempts)
}
