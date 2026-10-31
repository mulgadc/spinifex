package message

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
)

// ErrReceiptInvalid is a receipt handle that is malformed, unauthenticated,
// signed by an unknown key or issued for another queue. It never carries
// the handle.
var ErrReceiptInvalid = errors.New("message: receipt handle is invalid")

const (
	// ReceiptVersion is the handle layout this release signs and accepts.
	ReceiptVersion byte = 1
	// MinReceiptKeyLen is the shortest HMAC-SHA256 key a ring accepts.
	MinReceiptKeyLen = 32
	// MaxReceiptHandleLen bounds an encoded handle before it is decoded.
	MaxReceiptHandleLen = 1024

	receiptMACLen = sha256.Size
	// version, key ID, queue ID length, position, generation.
	receiptFixedLen = 1 + 4 + 1 + 8 + 8
)

var receiptEncoding = base64.RawURLEncoding.Strict()

// Receipt is what a handle binds: the queue, the message and the receive
// attempt that issued it (ADR-0012:S4).
type Receipt struct {
	Queue      QueueID
	Position   Position
	Generation uint64
}

// ReceiptKeys signs handles with one key and verifies them with any key it
// holds, so a rotation accepts the previous key. Key material is injected;
// it never appears in its formatted or logged value.
type ReceiptKeys struct {
	signing uint32
	keys    map[uint32][]byte
}

// NewReceiptKeys builds a ring that signs with the key named signing and
// verifies with every key given. Each key is at least MinReceiptKeyLen.
func NewReceiptKeys(signing uint32, keys map[uint32][]byte) (*ReceiptKeys, error) {
	if _, ok := keys[signing]; !ok {
		return nil, fmt.Errorf("%w: signing key %d not in ring", ErrInvalidArgument, signing)
	}
	held := make(map[uint32][]byte, len(keys))
	for id, k := range keys {
		if len(k) < MinReceiptKeyLen {
			return nil, fmt.Errorf("%w: receipt key %d shorter than %d bytes", ErrInvalidArgument, id, MinReceiptKeyLen)
		}
		held[id] = append([]byte(nil), k...)
	}
	return &ReceiptKeys{signing: signing, keys: held}, nil
}

// Check reports whether the ring holds a usable signing key; a zero-value
// or nil ring has none and must not sign.
func (r *ReceiptKeys) Check() error {
	if r == nil || len(r.keys[r.signing]) < MinReceiptKeyLen {
		return fmt.Errorf("%w: receipt ring has no usable signing key", ErrInvalidArgument)
	}
	return nil
}

// String names the signing key ID and the key count, never key bytes.
func (r *ReceiptKeys) String() string {
	return fmt.Sprintf("ReceiptKeys{signing: %d, keys: %d}", r.signing, len(r.keys))
}

// GoString is String, so %#v cannot print key bytes.
func (r *ReceiptKeys) GoString() string { return r.String() }

// LogValue logs the signing key ID and key count only.
func (r *ReceiptKeys) LogValue() slog.Value {
	return slog.GroupValue(slog.Any("signing", r.signing), slog.Int("keys", len(r.keys)))
}

// Encode issues the handle for a receive. The queue must be a valid ID and
// the position and generation non-zero.
func (r *ReceiptKeys) Encode(rc Receipt) (string, error) {
	if err := r.Check(); err != nil {
		return "", err
	}
	if err := (Namespace{Queue: rc.Queue}).Validate(); err != nil {
		return "", err
	}
	if rc.Position == 0 || rc.Generation == 0 {
		return "", fmt.Errorf("%w: receipt needs a position and a generation", ErrInvalidArgument)
	}
	qlen := len(rc.Queue)
	if qlen > math.MaxUint8 {
		return "", fmt.Errorf("%w: queue ID too long for a receipt", ErrInvalidArgument)
	}
	body := make([]byte, 0, receiptFixedLen+qlen+receiptMACLen)
	body = append(body, ReceiptVersion)
	body = binary.BigEndian.AppendUint32(body, r.signing)
	body = append(body, byte(qlen))
	body = append(body, rc.Queue...)
	body = binary.BigEndian.AppendUint64(body, uint64(rc.Position))
	body = binary.BigEndian.AppendUint64(body, rc.Generation)
	body = append(body, receiptMAC(r.keys[r.signing], body)...)
	return receiptEncoding.EncodeToString(body), nil
}

// Decode authenticates a handle and returns what it binds, refusing one
// issued for any queue but queue, including an earlier queue of the same
// name. Every refusal is ErrReceiptInvalid.
func (r *ReceiptKeys) Decode(handle string, queue QueueID) (Receipt, error) {
	if len(handle) > MaxReceiptHandleLen {
		return Receipt{}, ErrReceiptInvalid
	}
	b, err := receiptEncoding.DecodeString(handle)
	if err != nil || len(b) < receiptFixedLen+1+receiptMACLen || b[0] != ReceiptVersion {
		return Receipt{}, ErrReceiptInvalid
	}
	key, ok := r.keys[binary.BigEndian.Uint32(b[1:5])]
	qlen := int(b[5])
	if !ok || len(b) != receiptFixedLen+qlen+receiptMACLen {
		return Receipt{}, ErrReceiptInvalid
	}
	signed, mac := b[:len(b)-receiptMACLen], b[len(b)-receiptMACLen:]
	if !hmac.Equal(mac, receiptMAC(key, signed)) {
		return Receipt{}, ErrReceiptInvalid
	}
	rc := Receipt{
		Queue:      QueueID(b[6 : 6+qlen]),
		Position:   Position(binary.BigEndian.Uint64(b[6+qlen:])),
		Generation: binary.BigEndian.Uint64(b[14+qlen:]),
	}
	if rc.Queue != queue || rc.Position == 0 || rc.Generation == 0 {
		return Receipt{}, ErrReceiptInvalid
	}
	return rc, nil
}

// receiptMAC signs the version, queue ID, position and generation; the key
// ID only selects the key.
func receiptMAC(key, signed []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(signed[:1])
	m.Write(signed[5:])
	return m.Sum(nil)
}
