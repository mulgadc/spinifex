package handlers_ec2_volume

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata/vblegacy"
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

// seedEncryptedConfig writes a sealed (EncryptionEnabled=true) VBState to the store.
func seedEncryptedConfig(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string) {
	t.Helper()
	state := vblegacy.VBState{
		VolumeName:        volumeID,
		VolumeSize:        10 * 1024 * 1024 * 1024,
		BlockSize:         4096,
		EncryptionEnabled: true,
		VolumeConfig: vblegacy.VolumeConfig{
			VolumeMetadata: vblegacy.VolumeMetadata{
				VolumeID: volumeID,
				TenantID: testVolAccountID,
				SizeGiB:  10,
				State:    "available",
			},
		},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
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
