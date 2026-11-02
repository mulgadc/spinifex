package codestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
)

// MaxCodeBytes bounds one stored code copy, AWS's direct-upload ZIP limit.
const MaxCodeBytes = 50 << 20

var (
	// ErrCodeNotFound means no copy exists for the reference.
	ErrCodeNotFound = errors.New("lambda code copy not found")
	// ErrDigestMismatch means the stored bytes do not match the recorded SHA-256.
	ErrDigestMismatch = errors.New("lambda code copy digest mismatch")
	// ErrCodeTooLarge means the copy exceeds MaxCodeBytes.
	ErrCodeTooLarge = errors.New("lambda code copy too large")
	// ErrInvalidRef means a reference segment is empty or unsafe as a key segment.
	ErrInvalidRef = errors.New("invalid lambda code reference")
	// ErrNoPresignEndpoint means no off-host Predastore address is configured.
	ErrNoPresignEndpoint = errors.New("lambda code store: no off-host endpoint for download URLs")
)

var (
	refSegment = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`)
	digestHex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Ref names one code revision; keys group by account for S11 inventory.
type Ref struct {
	AccountID  string
	FunctionID string
	RevisionID string
}

func (r Ref) prefix() (string, error) {
	for _, seg := range []string{r.AccountID, r.FunctionID, r.RevisionID} {
		if !refSegment.MatchString(seg) {
			return "", fmt.Errorf("%w: %q", ErrInvalidRef, seg)
		}
	}
	return r.AccountID + "/" + r.FunctionID + "/" + r.RevisionID + "/", nil
}

// key names a copy by its digest, so different bytes for the same revision land
// on a different object and can never replace the recorded copy.
func (r Ref) key(sha256Hex string) (string, error) {
	p, err := r.prefix()
	if err != nil {
		return "", err
	}
	if !digestHex.MatchString(sha256Hex) {
		return "", fmt.Errorf("%w: digest %q", ErrInvalidRef, sha256Hex)
	}
	return p + sha256Hex + ".zip", nil
}

// Store reads and writes code copies in one Region's code bucket.
type Store struct {
	bucket  string
	objects objectstore.ObjectStore
	presign presignFunc

	// creds and provider are the scoped session behind presign; nil on stores
	// built without one.
	creds    *credentials.Credentials
	provider *sessionProvider
	now      func() time.Time
}

// presignMargin is slack kept between a signed URL's end and its session's.
const presignMargin = 30 * time.Second

// presignFunc signs a GET for the input; nil on stores that cannot presign.
type presignFunc func(in *s3.GetObjectInput, ttl time.Duration) (string, error)

// presignWith signs GETs with the session credentials behind client.
func presignWith(client *s3.S3) presignFunc {
	return func(in *s3.GetObjectInput, ttl time.Duration) (string, error) {
		req, _ := client.GetObjectRequest(in)
		return req.Presign(ttl)
	}
}

// OpenConfig is what the composition layer supplies to open the scoped store.
type OpenConfig struct {
	Region        string // Spinifex Region naming the bucket
	Endpoint      string // Predastore URL
	SigningRegion string // Predastore SigV4 Region
	Assume        AssumeFunc
	HTTPClient    *http.Client
	now           func() time.Time
	// PresignEndpoint is the Predastore URL off-host clients reach; download
	// URLs are signed for it. Empty disables download URLs.
	PresignEndpoint string
}

// Open builds the store on a refreshing code-store session and draws the first
// session, so an unassumable role fails here rather than on first use.
func Open(cfg OpenConfig) (*Store, error) {
	if cfg.Region == "" || cfg.Endpoint == "" || cfg.Assume == nil {
		return nil, errors.New("lambda code store: region, endpoint and assume are required")
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	provider := newSessionProvider(cfg.Assume, now)
	creds := credentials.NewCredentials(provider)
	if _, err := creds.Get(); err != nil {
		return nil, err
	}
	sess, err := session.NewSession(&aws.Config{
		Endpoint:         aws.String(cfg.Endpoint),
		Region:           aws.String(cfg.SigningRegion),
		Credentials:      creds,
		S3ForcePathStyle: aws.Bool(true),
		HTTPClient:       client,
	})
	if err != nil {
		return nil, fmt.Errorf("lambda code store session: %w", err)
	}
	s3Client := s3.New(sess)
	store := newStore(cfg.Region, objectstore.NewS3ObjectStore(s3Client))
	// Without an off-host address a download URL would name an internal one.
	if cfg.PresignEndpoint != "" {
		store.presign = presignWith(s3.New(sess, &aws.Config{Endpoint: aws.String(cfg.PresignEndpoint)}))
	}
	store.creds, store.provider, store.now = creds, provider, now
	return store, nil
}

func newStore(region string, objects objectstore.ObjectStore) *Store {
	return &Store{bucket: BucketName(region), objects: objects}
}

// Put stores code under its digest and returns the hex SHA-256 the revision
// record keeps. A retry with the same bytes rewrites identical content.
func (s *Store) Put(ctx context.Context, ref Ref, code []byte) (string, error) {
	if len(code) > MaxCodeBytes {
		return "", ErrCodeTooLarge
	}
	sum := sha256.Sum256(code)
	digest := hex.EncodeToString(sum[:])
	key, err := ref.key(digest)
	if err != nil {
		return "", err
	}
	if _, err := s.objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(code),
		ContentLength: aws.Int64(int64(len(code))),
	}); err != nil {
		return "", fmt.Errorf("put lambda code copy: %w", err)
	}
	return digest, nil
}

// Get returns the copy only if it matches wantSHA256, the recorded digest.
func (s *Store) Get(ctx context.Context, ref Ref, wantSHA256 string) ([]byte, error) {
	key, err := ref.key(wantSHA256)
	if err != nil {
		return nil, err
	}
	out, err := s.objects.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, ErrCodeNotFound
		}
		return nil, fmt.Errorf("get lambda code copy: %w", err)
	}
	defer out.Body.Close()
	code, err := io.ReadAll(io.LimitReader(out.Body, MaxCodeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read lambda code copy: %w", err)
	}
	if len(code) > MaxCodeBytes {
		return nil, ErrCodeTooLarge
	}
	sum := sha256.Sum256(code)
	if hex.EncodeToString(sum[:]) != wantSHA256 {
		return nil, ErrDigestMismatch
	}
	return code, nil
}

// ensureSession guarantees the signing session outlasts ttl, drawing a new one
// if it does not, and fails rather than sign a URL that would outlive it.
func (s *Store) ensureSession(ttl time.Duration) error {
	if s.creds == nil || s.provider == nil {
		return nil
	}
	need := ttl + presignMargin
	if _, err := s.creds.Get(); err != nil {
		return fmt.Errorf("lambda code store session: %w", err)
	}
	for attempt := range 2 {
		exp, err := s.creds.ExpiresAt()
		if err != nil {
			return fmt.Errorf("lambda code store session expiry: %w", err)
		}
		if !exp.Before(s.now().Add(need)) {
			return nil
		}
		if attempt == 1 {
			break
		}
		s.provider.requireRemaining(need)
		s.creds.Expire()
		if _, err := s.creds.Get(); err != nil {
			return fmt.Errorf("lambda code store session: %w", err)
		}
	}
	return errors.New("lambda code store: session expires before the presigned URL would")
}

// PresignGet returns a time-limited GET URL for the copy with the recorded
// digest, signed by a session that outlasts the URL.
func (s *Store) PresignGet(ctx context.Context, ref Ref, sha256Hex string, ttl time.Duration) (string, error) {
	if s.presign == nil {
		return "", ErrNoPresignEndpoint
	}
	key, err := ref.key(sha256Hex)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.ensureSession(ttl); err != nil {
		return "", err
	}
	url, err := s.presign(&s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}, ttl)
	if err != nil {
		return "", fmt.Errorf("presign lambda code copy: %w", err)
	}
	return url, nil
}

// Delete removes the copy with the recorded digest; an absent copy is already
// deleted.
func (s *Store) Delete(ctx context.Context, ref Ref, sha256Hex string) error {
	key, err := ref.key(sha256Hex)
	if err != nil {
		return err
	}
	return s.deleteKey(ctx, key)
}

// DeleteRevision removes every copy under the revision, including ones a failed
// or superseded attempt left. Callers own the guarantee that no create or update
// for the revision is still in progress.
func (s *Store) DeleteRevision(ctx context.Context, ref Ref) error {
	prefix, err := ref.prefix()
	if err != nil {
		return err
	}
	objects, _, err := objectstore.ListAll(ctx, s.objects, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(prefix),
	})
	if err != nil {
		return fmt.Errorf("list lambda code copies: %w", err)
	}
	for _, o := range objects {
		if err := s.deleteKey(ctx, aws.StringValue(o.Key)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) deleteKey(ctx context.Context, key string) error {
	_, err := s.objects.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil && !objectstore.IsNoSuchKeyError(err) {
		return fmt.Errorf("delete lambda code copy: %w", err)
	}
	return nil
}
