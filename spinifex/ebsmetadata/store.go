package ebsmetadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// Store persists Spinifex-owned metadata documents in the existing object
// store. It intentionally knows nothing about provider state or VBState.
type Store struct {
	objects objectstore.ObjectStore
	bucket  string
}

func NewStore(objects objectstore.ObjectStore, bucket string) *Store {
	return &Store{objects: objects, bucket: bucket}
}

// PutVolume takes no account: the key segment is the document's own TenantID.
// An account passed alongside a document that already carries one is how the
// two come to disagree.
func (s *Store) PutVolume(ctx context.Context, volume Volume) error {
	key, err := VolumeKey(volume.TenantID, volume.VolumeID)
	if err != nil {
		return err
	}
	data, err := MarshalVolume(volume)
	if err != nil {
		return fmt.Errorf("marshal volume metadata: %w", err)
	}
	return s.put(ctx, key, data)
}

// GetVolume returns the volume's ebsmetadata document. A volume with no
// document does not exist as far as the control plane is concerned.
func (s *Store) GetVolume(ctx context.Context, accountID, volumeID string) (Volume, error) {
	key, err := VolumeKey(accountID, volumeID)
	if err != nil {
		return Volume{}, err
	}
	data, err := s.get(ctx, key)
	if err != nil {
		return Volume{}, err
	}
	return UnmarshalVolume(data)
}

func (s *Store) DeleteVolume(ctx context.Context, accountID, volumeID string) error {
	key, err := VolumeKey(accountID, volumeID)
	if err != nil {
		return err
	}
	_, err = s.objects.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

// ListVolumes returns one account's volume documents, skipping any it cannot
// read or decode. The prefix is the isolation: no other account's document is
// read, so none has to be filtered out afterwards. One unreadable document must
// not make every volume invisible, so the tolerance is deliberate and logged.
func (s *Store) ListVolumes(ctx context.Context, accountID string) ([]Volume, error) {
	return s.listVolumeDocuments(ctx, accountID, true)
}

// ListVolumesStrict is ListVolumes without the tolerance. It is for callers
// whose answer would be wrong rather than merely partial: a volume that failed
// to read is not evidence that no volume holds the resource being checked.
func (s *Store) ListVolumesStrict(ctx context.Context, accountID string) ([]Volume, error) {
	return s.listVolumeDocuments(ctx, accountID, false)
}

// ListAllVolumes lists across every account. Whole-cluster by intent: the name
// is deliberately not ListVolumes so no caller gets this reach by leaving a
// call site untouched.
func (s *Store) ListAllVolumes(ctx context.Context) ([]Volume, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	return listDocuments(ctx, s, volumePrefix, "volume", UnmarshalVolume, true)
}

// ListAllVolumesStrict is ListAllVolumes without the tolerance.
func (s *Store) ListAllVolumesStrict(ctx context.Context) ([]Volume, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	return listDocuments(ctx, s, volumePrefix, "volume", UnmarshalVolume, false)
}

func (s *Store) listVolumeDocuments(ctx context.Context, accountID string, skipCorrupt bool) ([]Volume, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	prefix, err := accountPrefix(volumePrefix, accountID)
	if err != nil {
		return nil, err
	}
	return listDocuments(ctx, s, prefix, "volume", UnmarshalVolume, skipCorrupt)
}

// PutSnapshot takes no account: like PutVolume, the key segment is the
// document's own OwnerID.
func (s *Store) PutSnapshot(ctx context.Context, snapshot Snapshot) error {
	key, err := SnapshotKey(snapshot.OwnerID, snapshot.SnapshotID)
	if err != nil {
		return err
	}
	data, err := MarshalSnapshot(snapshot)
	if err != nil {
		return fmt.Errorf("marshal snapshot metadata: %w", err)
	}
	return s.put(ctx, key, data)
}

// GetSnapshot returns the snapshot's ebsmetadata document. A snapshot outside
// accountID's prefix is not found rather than denied: the key is the ownership
// check.
func (s *Store) GetSnapshot(ctx context.Context, accountID, snapshotID string) (Snapshot, error) {
	key, err := SnapshotKey(accountID, snapshotID)
	if err != nil {
		return Snapshot{}, err
	}
	data, err := s.get(ctx, key)
	if err != nil {
		return Snapshot{}, err
	}
	return UnmarshalSnapshot(data)
}

func (s *Store) DeleteSnapshot(ctx context.Context, accountID, snapshotID string) error {
	key, err := SnapshotKey(accountID, snapshotID)
	if err != nil {
		return err
	}
	_, err = s.objects.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

// ListSnapshots returns one account's snapshot documents, skipping any it
// cannot read or decode. The prefix is the isolation: no other account's
// document is read, so none has to be filtered out afterwards.
func (s *Store) ListSnapshots(ctx context.Context, accountID string) ([]Snapshot, error) {
	return s.listSnapshotDocuments(ctx, accountID, true)
}

// ListSnapshotsStrict is ListSnapshots without the tolerance, for callers whose
// answer would be wrong rather than merely partial.
func (s *Store) ListSnapshotsStrict(ctx context.Context, accountID string) ([]Snapshot, error) {
	return s.listSnapshotDocuments(ctx, accountID, false)
}

// ListAllSnapshots lists across every account. Whole-cluster by intent: the
// name is deliberately not ListSnapshots so no caller gets this reach by
// leaving a call site untouched.
func (s *Store) ListAllSnapshots(ctx context.Context) ([]Snapshot, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	return listDocuments(ctx, s, snapshotPrefix, "snapshot", UnmarshalSnapshot, true)
}

// ListAllSnapshotsStrict is ListAllSnapshots without the tolerance.
func (s *Store) ListAllSnapshotsStrict(ctx context.Context) ([]Snapshot, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	return listDocuments(ctx, s, snapshotPrefix, "snapshot", UnmarshalSnapshot, false)
}

func (s *Store) listSnapshotDocuments(ctx context.Context, accountID string, skipCorrupt bool) ([]Snapshot, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	prefix, err := accountPrefix(snapshotPrefix, accountID)
	if err != nil {
		return nil, err
	}
	return listDocuments(ctx, s, prefix, "snapshot", UnmarshalSnapshot, skipCorrupt)
}

// accountPrefix rejects the same accounts the key builders do, so a listing
// cannot widen to the whole tree by being handed an empty account.
func accountPrefix(kindPrefix, accountID string) (string, error) {
	if !utils.IsAccountID(accountID) {
		return "", fmt.Errorf("invalid EBS metadata account ID %q", accountID)
	}
	return kindPrefix + accountID + "/", nil
}

// ListAMIs returns every AMI document, skipping any it cannot decode. One
// corrupt document must not make every image in the cluster invisible, so the
// tolerance is deliberate — the skip is logged rather than silent.
func (s *Store) ListAMIs(ctx context.Context) ([]AMI, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	return s.listAMIDocuments(ctx, true)
}

// ListAMIsStrict is ListAMIs without the tolerance. It is for callers whose
// answer would be wrong rather than merely partial: a name-uniqueness check
// cannot rule out a name held by an AMI it failed to read.
func (s *Store) ListAMIsStrict(ctx context.Context) ([]AMI, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	return s.listAMIDocuments(ctx, false)
}

func (s *Store) listAMIDocuments(ctx context.Context, skipCorrupt bool) ([]AMI, error) {
	return listDocuments(ctx, s, amiPrefix, "AMI", UnmarshalAMI, skipCorrupt)
}

// Prefixes the metadata documents live under, one per document kind.
const (
	volumePrefix   = "spinifex/ebsmetadata/v2/volumes/"
	snapshotPrefix = "spinifex/ebsmetadata/v2/snapshots/"
	amiPrefix      = "spinifex/ebsmetadata/v2/amis/"
)

// listFetchConcurrency bounds the object fetches a single listing may have in
// flight. A listing costs one round trip per document, so fetching them one at
// a time makes the call scale with the number of documents in the bucket rather
// than with the answer's size.
const listFetchConcurrency = 16

// listFetchTimeout bounds how long one document may hold up a listing. A
// document whose shards no longer reassemble does not fail quickly — the read
// runs its full retry budget first — so without a bound a single bad document
// adds its whole latency to every listing that walks the prefix. It sits well
// under the request timeouts callers work to, and comfortably above a healthy
// read of a metadata document.
var listFetchTimeout = 10 * time.Second

// maxListPages bounds a single listing. It exists only to turn a server that
// keeps claiming truncation into an error rather than a hang; at the default
// page size it is ten million documents, far past any real prefix.
const maxListPages = 10000

// listKeys returns every key under prefix, following continuation tokens to
// exhaustion. Reading only the first page returns a prefix of the truth with no
// error, which for a strict caller is worse than a failure: absence from a
// truncated listing is not absence.
func listKeys(ctx context.Context, objects objectstore.ObjectStore, bucket, prefix string) ([]string, error) {
	var keys []string
	var token *string

	for range maxListPages {
		result, err := objects.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, object := range result.Contents {
			if object == nil || object.Key == nil {
				continue
			}
			keys = append(keys, *object.Key)
		}
		if !aws.BoolValue(result.IsTruncated) {
			return keys, nil
		}
		// Truncated with nothing to resume from. Returning what arrived is the
		// silent short answer this whole loop exists to prevent.
		if aws.StringValue(result.NextContinuationToken) == "" {
			return nil, fmt.Errorf("listing %s reported truncation with no continuation token", prefix)
		}
		token = result.NextContinuationToken
	}
	return nil, fmt.Errorf("listing %s did not finish within %d pages", prefix, maxListPages)
}

// listDocuments reads and decodes every document under prefix.
//
// skipCorrupt tolerates a document that cannot be fetched as well as one that
// cannot be decoded: an object whose shards no longer join is exactly as
// unusable as one whose bytes will not parse, and either kind read wholesale
// as a failure turns a single bad document into a cluster-wide outage.
//
// Fetches run concurrently but results are assembled in key order, so a strict
// listing fails on the same document it would have failed on serially.
func listDocuments[T any](
	ctx context.Context,
	s *Store,
	prefix, kind string,
	unmarshal func([]byte) (T, error),
	skipCorrupt bool,
) ([]T, error) {
	keys, err := listKeys(ctx, s.objects, s.bucket, prefix)
	if err != nil {
		return nil, err
	}

	// fetchErr and decodeErr are kept apart because they are tolerated on
	// different terms: any fetch failure may be skipped, but only a decode
	// failure that is ErrCorruptDocument may be.
	type fetched struct {
		key       string
		document  T
		fetchErr  error
		decodeErr error
	}

	results := make([]fetched, len(keys))
	slots := make(chan struct{}, listFetchConcurrency)
	var wg sync.WaitGroup

	for i, key := range keys {
		wg.Add(1)
		slots <- struct{}{}
		go func(idx int, key string) {
			defer wg.Done()
			defer func() { <-slots }()

			results[idx].key = key
			fetchCtx, cancel := context.WithTimeout(ctx, listFetchTimeout)
			defer cancel()
			data, err := s.get(fetchCtx, key)
			if err != nil {
				results[idx].fetchErr = err
				return
			}
			results[idx].document, results[idx].decodeErr = unmarshal(data)
		}(i, key)
	}
	wg.Wait()

	documents := make([]T, 0, len(results))
	for _, r := range results {
		switch {
		case r.fetchErr != nil:
			if !skipCorrupt {
				return nil, r.fetchErr
			}
			slog.WarnContext(ctx, "skipping unreadable "+kind+" document", "key", r.key, "err", r.fetchErr)
		case r.decodeErr != nil:
			if !skipCorrupt || !errors.Is(r.decodeErr, ErrCorruptDocument) {
				return nil, r.decodeErr
			}
			slog.WarnContext(ctx, "skipping undecodable "+kind+" document", "key", r.key, "err", r.decodeErr)
		default:
			documents = append(documents, r.document)
		}
	}
	return documents, nil
}

func (s *Store) PutAMI(ctx context.Context, ami AMI) error {
	key, err := AMIKey(ami.ImageID)
	if err != nil {
		return err
	}
	data, err := MarshalAMI(ami)
	if err != nil {
		return fmt.Errorf("marshal AMI metadata: %w", err)
	}
	return s.put(ctx, key, data)
}

// GetAMI returns the AMI's ebsmetadata document. An AMI with no document does
// not exist as far as the control plane is concerned.
func (s *Store) GetAMI(ctx context.Context, imageID string) (AMI, error) {
	key, err := AMIKey(imageID)
	if err != nil {
		return AMI{}, err
	}
	data, err := s.get(ctx, key)
	if err != nil {
		return AMI{}, err
	}
	return UnmarshalAMI(data)
}

func (s *Store) DeleteAMI(ctx context.Context, imageID string) error {
	key, err := AMIKey(imageID)
	if err != nil {
		return err
	}
	_, err = s.objects.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

func (s *Store) put(ctx context.Context, key string, data []byte) error {
	if s == nil || s.objects == nil {
		return errors.New("metadata store is not configured")
	}
	_, err := s.objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}

func (s *Store) get(ctx context.Context, key string) ([]byte, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	result, err := s.objects.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()
	return io.ReadAll(result.Body)
}
