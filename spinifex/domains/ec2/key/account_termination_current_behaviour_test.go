package key_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_AccountTermination_ tests pin how key pairs are
// stored and what a delete or Describe leaves behind today. They describe
// current behaviour, not the target design; a failure means it changed.

const (
	ctBucket  = "test-bucket"
	ctAccount = "111122223333"
	ctPubKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
)

var errInjected = errors.New("injected store failure")

// faultyStore fails the operations its predicates select and passes the rest
// to an in-memory store, standing in for a partial object-store outage.
type faultyStore struct {
	*objectstore.MemoryObjectStore

	failGet    func(key string) bool
	failPut    func(key string) bool
	failDelete func(key string) bool
}

var _ objectstore.ObjectStore = (*faultyStore)(nil)

func newFaultyStore() *faultyStore {
	return &faultyStore{MemoryObjectStore: objectstore.NewMemoryObjectStore()}
}

func (f *faultyStore) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if f.failGet != nil && f.failGet(aws.StringValue(in.Key)) {
		return nil, errInjected
	}
	return f.MemoryObjectStore.GetObject(ctx, in)
}

func (f *faultyStore) PutObject(ctx context.Context, in *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if f.failPut != nil && f.failPut(aws.StringValue(in.Key)) {
		return nil, errInjected
	}
	return f.MemoryObjectStore.PutObject(ctx, in)
}

func (f *faultyStore) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	if f.failDelete != nil && f.failDelete(aws.StringValue(in.Key)) {
		return nil, errInjected
	}
	return f.MemoryObjectStore.DeleteObject(ctx, in)
}

// accountObjects lists every object key under the account's key prefix.
func accountObjects(t *testing.T, store objectstore.ObjectStore, account string) []string {
	t.Helper()
	objects, _, err := objectstore.ListAll(context.Background(), store, &s3.ListObjectsV2Input{
		Bucket: aws.String(ctBucket),
		Prefix: aws.String("keys/" + account + "/"),
	})
	require.NoError(t, err)
	var keys []string
	for _, obj := range objects {
		keys = append(keys, aws.StringValue(obj.Key))
	}
	slices.Sort(keys)
	return keys
}

func readObject(t *testing.T, store objectstore.ObjectStore, key string) string {
	t.Helper()
	out, err := store.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(ctBucket), Key: aws.String(key)})
	require.NoError(t, err)
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	return string(body)
}

func putObject(t *testing.T, store objectstore.ObjectStore, key, body string) {
	t.Helper()
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(ctBucket), Key: aws.String(key), Body: strings.NewReader(body),
	})
	require.NoError(t, err)
}

func importKey(t *testing.T, svc *ec2key.KeyServiceImpl, account, name string) *ec2.ImportKeyPairOutput {
	t.Helper()
	out, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName: aws.String(name), PublicKeyMaterial: []byte(ctPubKey),
	}, account)
	require.NoError(t, err)
	return out
}

func describedKeyNames(t *testing.T, svc *ec2key.KeyServiceImpl, account string) []string {
	t.Helper()
	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{}, account)
	require.NoError(t, err)
	var names []string
	for _, kp := range out.KeyPairs {
		names = append(names, aws.StringValue(kp.KeyName))
	}
	slices.Sort(names)
	return names
}

// A key pair is two objects under keys/<account>/: the public key by name and
// the metadata by key pair ID. The generated private key is in neither.
func TestCurrentBehaviour_AccountTermination_KeyPairStorageLayout(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)

	imported := importKey(t, svc, ctAccount, "imported")
	created, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{KeyName: aws.String("created")}, ctAccount)
	require.NoError(t, err)
	importKey(t, svc, "444455556666", "other-account")

	prefix := "keys/" + ctAccount + "/"
	want := []string{
		prefix + "created",
		prefix + "imported",
		prefix + aws.StringValue(created.KeyPairId) + ".json",
		prefix + aws.StringValue(imported.KeyPairId) + ".json",
	}
	slices.Sort(want)
	assert.Equal(t, want, accountObjects(t, store, ctAccount))

	assert.Contains(t, readObject(t, store, prefix+"imported"), ctPubKey)
	meta := readObject(t, store, prefix+aws.StringValue(created.KeyPairId)+".json")
	assert.Contains(t, meta, `"KeyName":"created"`)
	assert.NotContains(t, meta, "PRIVATE KEY")
	for _, key := range accountObjects(t, store, ctAccount) {
		assert.NotContains(t, readObject(t, store, key), aws.StringValue(created.KeyMaterial))
	}
}

// Pins current behaviour that differs from AWS: a failed public-key delete is
// logged, the metadata delete proceeds and the call reports success. The
// leftover object still blocks the name and passes launch validation.
func TestCurrentBehaviour_AccountTermination_DeleteKeyPairLeavesPublicKeyWhenItsDeleteFails(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	imported := importKey(t, svc, ctAccount, "deploy")
	pubKeyPath := "keys/" + ctAccount + "/deploy"
	store.failDelete = func(key string) bool { return key == pubKeyPath }

	out, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{KeyName: aws.String("deploy")}, ctAccount)
	require.NoError(t, err)
	assert.True(t, aws.BoolValue(out.Return))
	assert.Equal(t, aws.StringValue(imported.KeyPairId), aws.StringValue(out.KeyPairId))

	assert.Equal(t, []string{pubKeyPath}, accountObjects(t, store, ctAccount), "only the public key object remains")
	assert.Empty(t, describedKeyNames(t, svc, ctAccount))
	assert.NoError(t, svc.ValidateKeyPairExists(context.Background(), ctAccount, "deploy"))

	// A retried delete finds no metadata, reports success and removes nothing.
	store.failDelete = nil
	_, err = svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{KeyName: aws.String("deploy")}, ctAccount)
	require.NoError(t, err)
	assert.Equal(t, []string{pubKeyPath}, accountObjects(t, store, ctAccount))

	_, err = svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{KeyName: aws.String("deploy")}, ctAccount)
	require.Error(t, err)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairDuplicate, code)
}

// A create whose metadata write fails and whose cleanup delete also fails
// returns an error and leaves the public key object with no metadata.
func TestCurrentBehaviour_AccountTermination_FailedCreateCleanupLeavesPublicKey(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	store.failPut = func(key string) bool { return strings.HasSuffix(key, ".json") }
	store.failDelete = func(string) bool { return true }

	_, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{KeyName: aws.String("half")}, ctAccount)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())

	assert.Equal(t, []string{"keys/" + ctAccount + "/half"}, accountObjects(t, store, ctAccount))
	assert.Empty(t, describedKeyNames(t, svc, ctAccount))
}

// DescribeKeyPairs reports only metadata it can read and decode, and never a
// public key object on its own. Each record class below is skipped silently
// and the call still succeeds.
func TestCurrentBehaviour_AccountTermination_DescribeKeyPairsHidesUnreadableRecords(t *testing.T) {
	prefix := "keys/" + ctAccount + "/"
	tests := []struct {
		name string
		seed func(t *testing.T, store *faultyStore)
	}{
		{"public key without metadata", func(t *testing.T, store *faultyStore) {
			putObject(t, store, prefix+"orphan", ctPubKey+"\n")
		}},
		{"undecodable metadata", func(t *testing.T, store *faultyStore) {
			putObject(t, store, prefix+"key-0badbadbadbadbad0.json", "{not json")
		}},
		{"metadata the store cannot read", func(t *testing.T, store *faultyStore) {
			putObject(t, store, prefix+"key-0unreadable000000.json", `{"KeyName":"hidden","KeyPairId":"key-0unreadable000000"}`)
			store.failGet = func(key string) bool { return key == prefix+"key-0unreadable000000.json" }
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFaultyStore()
			svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
			importKey(t, svc, ctAccount, "live")
			tc.seed(t, store)

			assert.Len(t, accountObjects(t, store, ctAccount), 3, "the seeded record is in the owner store")
			assert.Equal(t, []string{"live"}, describedKeyNames(t, svc, ctAccount))
		})
	}
}
