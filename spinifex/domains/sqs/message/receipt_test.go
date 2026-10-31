package message_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	keyA = bytes.Repeat([]byte{0xA1}, message.MinReceiptKeyLen)
	keyB = bytes.Repeat([]byte{0xB2}, message.MinReceiptKeyLen)
)

func ring(t *testing.T, signing uint32, keys map[uint32][]byte) *message.ReceiptKeys {
	t.Helper()
	r, err := message.NewReceiptKeys(signing, keys)
	require.NoError(t, err)
	return r
}

var receiptA = message.Receipt{Queue: "QUEUEAAAA", Position: 42, Generation: 3}

func TestReceiptRoundTrip(t *testing.T) {
	r := ring(t, 1, map[uint32][]byte{1: keyA})
	for _, rc := range []message.Receipt{
		receiptA,
		{Queue: "q", Position: 1, Generation: 1},
		{Queue: message.QueueID(strings.Repeat("Z", message.MaxQueueIDLen)), Position: ^message.Position(0), Generation: ^uint64(0)},
	} {
		h, err := r.Encode(rc)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(h), message.MaxReceiptHandleLen)
		got, err := r.Decode(h, rc.Queue)
		require.NoError(t, err)
		assert.Equal(t, rc, got)
	}
}

// Changing any byte, including the version and key ID, leaves a handle
// that no key in the ring accepts.
func TestReceiptRejectsEveryTamperedByte(t *testing.T) {
	r := ring(t, 1, map[uint32][]byte{1: keyA, 2: keyB})
	h, err := r.Encode(receiptA)
	require.NoError(t, err)
	raw, err := base64.RawURLEncoding.DecodeString(h)
	require.NoError(t, err)
	for i := range raw {
		for _, flip := range []byte{0x01, 0x80} {
			b := bytes.Clone(raw)
			b[i] ^= flip
			_, err := r.Decode(base64.RawURLEncoding.EncodeToString(b), receiptA.Queue)
			require.ErrorIs(t, err, message.ErrReceiptInvalid, "byte %d ^ %#x", i, flip)
		}
	}
}

// A forger who knows the layout but not the key cannot mint a handle, even
// for an existing message and the current generation.
func TestReceiptRejectsForgery(t *testing.T) {
	r := ring(t, 1, map[uint32][]byte{1: keyA})
	attacker := ring(t, 1, map[uint32][]byte{1: keyB})
	h, err := attacker.Encode(receiptA)
	require.NoError(t, err)
	_, err = r.Decode(h, receiptA.Queue)
	require.ErrorIs(t, err, message.ErrReceiptInvalid)
}

func TestReceiptRejectsMalformed(t *testing.T) {
	r := ring(t, 1, map[uint32][]byte{1: keyA})
	h, err := r.Encode(receiptA)
	require.NoError(t, err)
	raw, err := base64.RawURLEncoding.DecodeString(h)
	require.NoError(t, err)
	lenAt := func(n byte) string {
		b := bytes.Clone(raw)
		b[5] = n
		return base64.RawURLEncoding.EncodeToString(b)
	}
	for name, bad := range map[string]string{
		"empty":            "",
		"not base64":       "!!!!",
		"padded":           h + "==",
		"whitespace":       h[:8] + " " + h[8:],
		"truncated":        h[:len(h)-4],
		"extended":         h + "AAAA",
		"too long":         strings.Repeat("A", message.MaxReceiptHandleLen+1),
		"zero queue len":   lenAt(0),
		"queue len short":  lenAt(byte(len(receiptA.Queue) - 1)),
		"queue len long":   lenAt(byte(len(receiptA.Queue) + 1)),
		"only fixed bytes": base64.RawURLEncoding.EncodeToString(raw[:6]),
	} {
		_, err := r.Decode(bad, receiptA.Queue)
		require.ErrorIs(t, err, message.ErrReceiptInvalid, name)
	}
}

// forge builds a handle with a valid MAC for any version, so a test can
// separate the version check from the MAC check.
func forge(key []byte, v byte, keyID uint32, rc message.Receipt) string {
	b := []byte{v}
	b = binary.BigEndian.AppendUint32(b, keyID)
	b = append(b, byte(len(rc.Queue)))
	b = append(b, rc.Queue...)
	b = binary.BigEndian.AppendUint64(b, uint64(rc.Position))
	b = binary.BigEndian.AppendUint64(b, rc.Generation)
	mac := hmacSHA256(key, append([]byte{v}, b[5:]...))
	return base64.RawURLEncoding.EncodeToString(append(b, mac...))
}

// Rotation: the new ring signs with key 2 and still accepts key 1's handles
// until key 1 is dropped; a key the ring never held is refused.
func TestReceiptKeyRotation(t *testing.T) {
	old := ring(t, 1, map[uint32][]byte{1: keyA})
	issued, err := old.Encode(receiptA)
	require.NoError(t, err)

	rotated := ring(t, 2, map[uint32][]byte{1: keyA, 2: keyB})
	got, err := rotated.Decode(issued, receiptA.Queue)
	require.NoError(t, err, "the previous key is accepted")
	assert.Equal(t, receiptA, got)
	fresh, err := rotated.Encode(receiptA)
	require.NoError(t, err)
	assert.NotEqual(t, issued, fresh)

	_, err = old.Decode(fresh, receiptA.Queue)
	require.ErrorIs(t, err, message.ErrReceiptInvalid, "a ring without key 2 refuses its handles")

	retired := ring(t, 2, map[uint32][]byte{2: keyB})
	_, err = retired.Decode(issued, receiptA.Queue)
	require.ErrorIs(t, err, message.ErrReceiptInvalid, "a dropped key is refused")

	// Same bytes under another key ID: the key ID selects the key, so it
	// cannot redirect verification to a key the handle was not signed with.
	_, err = rotated.Decode(forge(keyA, message.ReceiptVersion, 2, receiptA), receiptA.Queue)
	require.ErrorIs(t, err, message.ErrReceiptInvalid)
}

// A queue deleted and recreated under its name gets a new queue ID, so the
// predecessor's handles are refused; so is any other queue's handle.
func TestReceiptRejectsAnotherQueue(t *testing.T) {
	r := ring(t, 1, map[uint32][]byte{1: keyA})
	h, err := r.Encode(receiptA)
	require.NoError(t, err)
	for _, q := range []message.QueueID{"QUEUEBBBB", "QUEUEAAAAA", "QUEUEAAA", "queueaaaa", ""} {
		_, err := r.Decode(h, q)
		require.ErrorIs(t, err, message.ErrReceiptInvalid, "queue %q", q)
	}
}

// Neither keys nor handles reach formatted output, logs or errors.
func TestReceiptSecretsStayOutOfOutput(t *testing.T) {
	r := ring(t, 1, map[uint32][]byte{1: keyA, 2: keyB})
	h, err := r.Encode(receiptA)
	require.NoError(t, err)

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("ring", "keys", r)
	_, decErr := r.Decode(h+"x", receiptA.Queue)
	require.Error(t, decErr)

	out := strings.Join([]string{
		fmt.Sprint(r), fmt.Sprintf("%v %+v %#v %s", r, r, r, r), logs.String(), decErr.Error(),
	}, "\n")
	for _, k := range [][]byte{keyA, keyB} {
		for _, enc := range []string{string(k), hex.EncodeToString(k), fmt.Sprintf("%d", k), fmt.Sprintf("%#v", k), base64.StdEncoding.EncodeToString(k)} {
			assert.NotContains(t, out, enc)
		}
	}
	assert.NotContains(t, out, "0xa1, 0xa1")
	assert.NotContains(t, out, "0xb2, 0xb2")
	assert.NotContains(t, out, h)
	assert.Contains(t, logs.String(), `"signing":1`)
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}
