package function

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// List bounds: a page returns at most MaxListPage functions and examines at
// most maxListScan stream messages, delete markers included.
const (
	MaxListPage = 50
	maxListScan = 200
)

// listCursor is the decoded marker: the last stream sequence a page examined,
// bound to the account and Region it was issued for.
type listCursor struct {
	Version   int    `json:"v"`
	AccountID string `json:"a"`
	Region    string `json:"r"`
	After     uint64 `json:"s"`
}

func encodeCursor(c listCursor) (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeCursor(marker, accountID, region string) (uint64, error) {
	bad := awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "Invalid marker: %s", marker)
	data, err := base64.RawURLEncoding.DecodeString(marker)
	if err != nil {
		return 0, bad
	}
	var c listCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return 0, bad
	}
	if c.Version != 1 || c.AccountID != accountID || c.Region != region || c.After == 0 || c.After == math.MaxUint64 {
		return 0, bad
	}
	return c.After, nil
}

// List returns the Region's functions in stream order after the marker. Each
// step is one leader message get filtered to this Region's function keys, so a
// page costs at most its scan bound; NextMarker is set unless the walk ended.
func (s *MetaService) List(ctx context.Context, req *ListRequest, accountID string) (*ListResponse, error) {
	if !regionRE.MatchString(req.Region) {
		return nil, badRegion()
	}
	if accountID == "" || req.MaxItems < 1 || req.MaxItems > MaxListPage {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "list request is incomplete")
	}
	var after uint64
	if req.Marker != "" {
		var err error
		if after, err = decodeCursor(req.Marker, accountID, req.Region); err != nil {
			return nil, err
		}
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	bucket := kv.Bucket()
	keyPrefix := "$KV." + bucket + "." + kvFunctionKeyPrefix + req.Region + "."
	sel := kvstore.MsgGetRequest{NextFor: keyPrefix + ">"}
	out := &ListResponse{Functions: []ListedFunction{}}
	for range maxListScan {
		sel.Seq = after + 1
		m, err := kvstore.LeaderMsgGet(ctx, s.js, "KV_"+bucket, sel)
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("lambda: list function records: %w", err)
		}
		if m.Sequence <= after || !strings.HasPrefix(m.Subject, keyPrefix) {
			return nil, fmt.Errorf("lambda: list function records: %w", jetstream.ErrInvalidJetStreamResponse)
		}
		gone := removed(m.Header)
		if !gone && len(out.Functions) == req.MaxItems {
			// A full page looked ahead to a live record it cannot return; the
			// marker stays before it.
			break
		}
		after = m.Sequence
		if gone {
			continue
		}
		rec, err := decode(m.Data)
		if err != nil {
			return nil, err
		}
		out.Functions = append(out.Functions, ListedFunction{Record: rec, Revision: m.Sequence})
	}
	if out.NextMarker, err = encodeCursor(listCursor{Version: 1, AccountID: accountID, Region: req.Region, After: after}); err != nil {
		return nil, err
	}
	return out, nil
}

// removed reports a key's delete or purge marker, as the KV Get of nats.go
// reads one.
func removed(h nats.Header) bool {
	if op := h.Get("KV-Operation"); op != "" {
		return op == "DEL" || op == "PURGE"
	}
	switch h.Get(jetstream.MarkerReasonHeader) {
	case "MaxAge", "Purge", "Remove":
		return true
	}
	return false
}
