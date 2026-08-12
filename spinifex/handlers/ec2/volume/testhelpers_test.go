package handlers_ec2_volume

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// startTestNATS spins up an in-process NATS server and returns a connected client.
func startTestNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })
	return nc
}

// seedVolumeDocument writes the control-plane document that makes a volume
// exist as far as the control plane is concerned.
func seedVolumeDocument(t *testing.T, store objectstore.ObjectStore, volume ebsmetadata.Volume) {
	t.Helper()
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
