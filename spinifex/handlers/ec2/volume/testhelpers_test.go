package handlers_ec2_volume

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// startTestNATS spins up an in-process NATS server and returns a connected client.
func startTestNATS(t *testing.T) *nats.Conn {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return nc
}

// seedVolumeDocument writes the control-plane document that makes a volume
// exist as far as the control plane is concerned.
func seedVolumeDocument(t *testing.T, store objectstore.ObjectStore, volume ebsmetadata.Volume) {
	t.Helper()
	// The owning account is a key segment, so a fixture that does not care
	// which account owns it still has to have one.
	if volume.TenantID == "" {
		volume.TenantID = testVolAccountID
	}
	require.NoError(t, ebsmetadata.NewStore(store, "test-bucket").PutVolume(context.Background(), volume))
}

// seedProviderConfig writes provider-owned state at the volume's config.json.
// Its contents are deliberately opaque here: the control plane must neither
// read nor rewrite them, so the tests only care that the bytes survive.
func seedProviderConfig(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string) {
	t.Helper()
	putProviderConfig(t, store, volumeID, `{"VolumeName":"`+volumeID+`","VolumeSize":10737418240,"BlockSize":4096,"SeqNum":7,"State":"available"}`)
}

// seedEncryptedConfig writes a sealed provider config.json: an opaque
// ciphertext blob a rewrite would corrupt rather than merely overwrite.
func seedEncryptedConfig(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string) {
	t.Helper()
	putProviderConfig(t, store, volumeID, `{"EncryptionEnabled":true,"StateSeqNum":7,"Sealed":"3q2+7wAAAAAAAAAAAAAAAA=="}`)
}

func putProviderConfig(t *testing.T, store *objectstore.MemoryObjectStore, volumeID, body string) {
	t.Helper()
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(body),
	})
	require.NoError(t, err)
}

// getStoredConfig reads the raw config.json bytes from the memory store.
func getStoredConfig(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string) []byte {
	t.Helper()
	res, err := store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
	})
	require.NoError(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return data
}
