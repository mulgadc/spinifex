package key

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
)

// AccountScan is a read-only classification of every object under one
// account's key prefix. It holds key-pair IDs and counts only; object keys,
// key names and object bodies never leave the scan.
type AccountScan struct {
	// LiveKeyPairIDs are metadata records DescribeKeyPairs reports.
	LiveKeyPairIDs []string
	// UnreadableKeyPairIDs are metadata records found but not read or decoded.
	UnreadableKeyPairIDs []string
	// OrphanPublicKeys are public-key objects with no live metadata naming them.
	OrphanPublicKeys int
	// UnresolvedPublicKeys are public-key objects that may belong to unreadable
	// metadata, so they cannot be called residue.
	UnresolvedPublicKeys int
	// ReadFailures counts objects the store failed to return.
	ReadFailures int
}

// liveKeyPair is a readable metadata record and the name it gives its key.
type liveKeyPair struct {
	id, name string
}

// orphanObject is a public-key object no metadata names, the key name it is
// served under, and its last write time when the listing reported one.
type orphanObject struct {
	key, name string
	modified  *time.Time
}

// accountRecords is the private classification ScanAccount and CleanupAccount
// share. It holds object keys and names, so it never leaves this package.
type accountRecords struct {
	live         []liveKeyPair
	unreadable   []string
	orphans      []orphanObject
	unresolved   int
	readFailures int
}

// ScanAccount lists and classifies every object under keys/<accountID>/. A
// listing failure is returned as an error; per-object read failures are
// counted. It reads only objects named as metadata (key-*.json).
func (s *KeyServiceImpl) ScanAccount(ctx context.Context, accountID string) (*AccountScan, error) {
	records, err := s.classifyAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	scan := &AccountScan{
		UnreadableKeyPairIDs: records.unreadable,
		OrphanPublicKeys:     len(records.orphans),
		UnresolvedPublicKeys: records.unresolved,
		ReadFailures:         records.readFailures,
	}
	for _, kp := range records.live {
		scan.LiveKeyPairIDs = append(scan.LiveKeyPairIDs, kp.id)
	}
	return scan, nil
}

func (s *KeyServiceImpl) classifyAccount(ctx context.Context, accountID string) (*accountRecords, error) {
	prefix := fmt.Sprintf("keys/%s/", accountID)
	objects, _, err := objectstore.ListAll(ctx, s.store, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucketName),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return nil, fmt.Errorf("list key-pair objects: %w", err)
	}

	records := &accountRecords{}
	liveNames := make(map[string]bool)
	var rest []*s3.Object
	unreadable := make(map[string]bool)
	for _, obj := range objects {
		if obj == nil || obj.Key == nil {
			continue
		}
		name := strings.TrimPrefix(*obj.Key, prefix)
		id, isJSON := strings.CutSuffix(name, ".json")
		if !isJSON || !strings.HasPrefix(id, "key-") {
			rest = append(rest, obj)
			continue
		}
		metadata, found, readErr := s.readScanMetadata(ctx, *obj.Key)
		switch {
		case readErr != nil:
			records.readFailures++
			unreadable[name] = true
		case !found:
			// Removed between the listing and the read.
		case metadata == nil || aws.StringValue(metadata.KeyPairId) != id || aws.StringValue(metadata.KeyName) == "":
			unreadable[name] = true
		default:
			records.live = append(records.live, liveKeyPair{id: id, name: *metadata.KeyName})
			liveNames[*metadata.KeyName] = true
		}
		if unreadable[name] {
			rest = append(rest, obj)
		}
	}

	// A .json object that is the public key of a live key pair named *.json is
	// not unreadable metadata, so live names are settled before anything else.
	for name := range unreadable {
		if liveNames[name] {
			delete(unreadable, name)
		}
	}
	for _, obj := range rest {
		name := strings.TrimPrefix(*obj.Key, prefix)
		switch {
		case liveNames[name]:
			// The public key of a live key pair.
		case unreadable[name]:
			records.unreadable = append(records.unreadable, strings.TrimSuffix(name, ".json"))
		case len(unreadable) > 0:
			records.unresolved++
		default:
			records.orphans = append(records.orphans, orphanObject{key: *obj.Key, name: name, modified: obj.LastModified})
		}
	}
	return records, nil
}

// readScanMetadata reads and decodes one metadata object. A decode failure is
// reported as a nil record, not an error, and its text is discarded because a
// decoder message can quote the stored bytes.
func (s *KeyServiceImpl) readScanMetadata(ctx context.Context, objectKey string) (*keyPairMetadata, bool, error) {
	out, err := s.store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, false, nil
		}
		slog.WarnContext(ctx, "Key-pair account scan: metadata read failed", "err", err)
		return nil, true, err
	}
	body, err := io.ReadAll(out.Body)
	if closeErr := out.Body.Close(); closeErr != nil {
		slog.DebugContext(ctx, "Key-pair account scan: metadata close failed", "err", closeErr)
	}
	if err != nil {
		slog.WarnContext(ctx, "Key-pair account scan: metadata body read failed", "err", err)
		return nil, true, err
	}
	if metadata, decodeErr := decodeKeyPairMetadata(body); decodeErr == nil {
		return metadata, true, nil
	}
	return nil, true, nil
}
