package types

// StorageConfigResponse is returned by the spinifex.storage.config NATS topic.
// Contains predastore topology with credentials stripped.
type StorageConfigResponse struct {
	Encoding  StorageEncoding   `json:"encoding"`
	MetaNodes []StorageMetaNode `json:"meta_nodes"`
	BlobNodes []StorageBlobNode `json:"blob_nodes"`
	Buckets   []StorageBucket   `json:"buckets"`
}

// StorageEncoding describes the Reed-Solomon erasure coding configuration.
type StorageEncoding struct {
	DataShards   int `json:"data_shards"`
	ParityShards int `json:"parity_shards"`
}

// StorageMetaNode describes a predastore meta node, one member of the Raft
// quorum over global state (no credentials).
type StorageMetaNode struct {
	ID   int    `json:"id"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// StorageBlobNode describes a predastore blob node, which holds erasure-coded
// object shards.
type StorageBlobNode struct {
	ID   int    `json:"id"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// StorageBucket describes a configured S3 bucket.
type StorageBucket struct {
	Name   string `json:"name"`
	Region string `json:"region"`
}
