package operatorv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorageConfigResponseJSONContract(t *testing.T) {
	data, err := json.Marshal(StorageConfigResponse{
		Encoding:  StorageEncoding{DataShards: 2, ParityShards: 1},
		MetaNodes: []StorageMetaNode{{ID: 1, Host: "10.0.0.1", Port: 7660}},
		BlobNodes: []StorageBlobNode{{ID: 2, Host: "10.0.0.1", Port: 6660}},
		Buckets:   []StorageBucket{{Name: "objects", Region: "ap-southeast-2"}},
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"encoding":{"data_shards":2,"parity_shards":1},
		"meta_nodes":[{"id":1,"host":"10.0.0.1","port":7660}],
		"blob_nodes":[{"id":2,"host":"10.0.0.1","port":6660}],
		"buckets":[{"name":"objects","region":"ap-southeast-2"}]
	}`, string(data))
}

func TestStorageConfigSubject(t *testing.T) {
	require.Equal(t, "spinifex.storage.config", StorageConfigSubject)
}
