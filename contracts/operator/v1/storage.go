// Package operatorv1 defines version 1 of cross-process operator-plane
// contracts. These are deployment-administration interfaces, not AWS
// tenant-facing APIs.
package operatorv1

const (
	// StorageConfigSubject returns the current Predastore topology as configured
	// for this deployment. It is consumed by the SPX operator status path.
	StorageConfigSubject = "spinifex.storage.config"
)

// StorageConfigResponse is the operator-plane response on
// StorageConfigSubject. It intentionally describes the current Predastore
// implementation (Reed-Solomon and meta/blob roles); it is not a generic
// storage-provider contract.
type StorageConfigResponse struct {
	Encoding  StorageEncoding   `json:"encoding"`
	MetaNodes []StorageMetaNode `json:"meta_nodes"`
	BlobNodes []StorageBlobNode `json:"blob_nodes"`
	Buckets   []StorageBucket   `json:"buckets"`
}

// StorageEncoding describes Predastore's Reed-Solomon erasure coding
// configuration.
type StorageEncoding struct {
	DataShards   int `json:"data_shards"`
	ParityShards int `json:"parity_shards"`
}

// StorageMetaNode describes a Predastore meta node, one member of the Raft
// quorum over global state. Credentials are deliberately excluded.
type StorageMetaNode struct {
	ID   int    `json:"id"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// StorageBlobNode describes a Predastore blob node that holds erasure-coded
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
