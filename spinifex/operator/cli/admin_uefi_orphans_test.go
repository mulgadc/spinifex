package cli

import (
	"bytes"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon"
)

func putKeys(t *testing.T, js jetstream.JetStream, bucket string, keys ...string) {
	t.Helper()
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)
	for _, key := range keys {
		_, err := kv.Put(t.Context(), key, []byte("not decoded"))
		require.NoError(t, err)
	}
}

// Every key space an instance record has lived in counts, and the node
// blobs and presence markers name nodes rather than instances.
func TestKVInstanceIndex_CountsEveryInstanceKeySpace(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	putKeys(t, js, daemon.InstanceStateBucket, "i.i-running", "instance.i-frozenstopped", "node.node-1", "nodepresence.node-1")
	putKeys(t, js, daemon.TerminatedInstanceBucket, "i.i-terminated", "terminated.i-frozenterminated")

	ids, err := kvInstanceIndex{js: js}.InstanceIDs(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{
		"i-running": true, "i-frozenstopped": true, "i-terminated": true, "i-frozenterminated": true,
	}, ids)
}

// The report attaches to the buckets rather than creating them, so a cluster
// missing one fails the scan instead of reporting every store as unowned.
func TestReportUEFIStoreOrphans_FailsWhenABucketIsMissing(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	objects := objectstore.NewMemoryObjectStore()

	require.Error(t, reportUEFIStoreOrphans(t.Context(), nc, objects, "spinifex"))
}

func TestReportUEFIStoreOrphans_PrintsAndDeletesNothing(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	putKeys(t, js, daemon.InstanceStateBucket)
	putKeys(t, js, daemon.TerminatedInstanceBucket)
	stores := uefistore.NewStore(js)
	require.NoError(t, stores.Open(t.Context()))
	_, err := stores.Create(t.Context(), &uefistore.UEFIStoreRecord{InstanceID: "i-0000000000recorded", AccountID: "111122223333", State: uefistore.StateLive})
	require.NoError(t, err)
	objects := objectstore.NewMemoryObjectStore()
	for _, owner := range []string{"i-00000000000noowner", "ami-000000000noimage", "vol-00000000000other"} {
		_, err := objects.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("spinifex"), Key: aws.String(uefistore.KeyPrefix(owner) + "a"), Body: bytes.NewReader([]byte("vars")),
		})
		require.NoError(t, err)
	}

	out := captureStdout(t, func() {
		require.NoError(t, reportUEFIStoreOrphans(t.Context(), nc, objects, "spinifex"))
	})

	assert.Contains(t, out, "uefi/i-00000000000noowner/")
	assert.Contains(t, out, "uefi/ami-000000000noimage/")
	assert.Contains(t, out, "uefi/vol-00000000000other/")
	assert.Contains(t, out, "store record i-0000000000recorded")
	assert.Contains(t, out, "Nothing was deleted")
	assert.Equal(t, 3, objects.Count())
}

func TestReportUEFIStoreOrphans_CleanCluster(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	putKeys(t, js, daemon.InstanceStateBucket)
	putKeys(t, js, daemon.TerminatedInstanceBucket)
	require.NoError(t, uefistore.NewStore(js).Open(t.Context()))

	out := captureStdout(t, func() {
		require.NoError(t, reportUEFIStoreOrphans(t.Context(), nc, objectstore.NewMemoryObjectStore(), "spinifex"))
	})
	assert.Contains(t, out, "No orphaned UEFI variable stores.")
}
