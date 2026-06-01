package handlers_eks

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// ClusterStatus is the persisted lifecycle state for a cluster. Values match
// the AWS EKS Cluster.status string set so DescribeCluster can pass them
// through verbatim.
type ClusterStatus string

const (
	ClusterStatusCreating ClusterStatus = "CREATING"
	ClusterStatusActive   ClusterStatus = "ACTIVE"
	ClusterStatusDeleting ClusterStatus = "DELETING"
	ClusterStatusFailed   ClusterStatus = "FAILED"
)

// ClusterVpcConfig is the persisted subset of eks.VpcConfigResponse that the
// reconciler + DescribeCluster need at-rest.
type ClusterVpcConfig struct {
	SubnetIds        []string `json:"subnetIds"`
	SecurityGroupIds []string `json:"securityGroupIds,omitempty"`
	VpcId            string   `json:"vpcId,omitempty"`
}

// ClusterMeta is the persisted control-plane record for one cluster. The
// blob lives at ClusterMetaKey(name) inside the per-account KV bucket and is
// the source of truth for DescribeCluster.
type ClusterMeta struct {
	Name                    string            `json:"name"`
	Arn                     string            `json:"arn"`
	Status                  ClusterStatus     `json:"status"`
	Version                 string            `json:"version"`
	RoleArn                 string            `json:"roleArn"`
	Endpoint                string            `json:"endpoint,omitempty"`
	OIDCIssuer              string            `json:"oidcIssuer,omitempty"`
	CertificateAuthorityB64 string            `json:"certificateAuthorityB64,omitempty"`
	ResourcesVpcConfig      *ClusterVpcConfig `json:"resourcesVpcConfig,omitempty"`
	ControlPlaneInstanceID  string            `json:"controlPlaneInstanceId,omitempty"`
	ControlPlaneENIID       string            `json:"controlPlaneEniId,omitempty"`
	ControlPlaneENIIP       string            `json:"controlPlaneEniIp,omitempty"`
	NLBArn                  string            `json:"nlbArn,omitempty"`
	NLBTargetGroupArn       string            `json:"nlbTargetGroupArn,omitempty"`
	CreatedAt               time.Time         `json:"createdAt"`
}

// ErrClusterNotFound is returned by GetClusterMeta / SetClusterStatus /
// DeleteClusterPrefix when the cluster meta key is absent. Callers translate
// to the AWS shape (ResourceNotFoundException) at the service boundary.
var ErrClusterNotFound = errors.New("eks: cluster not found")

const maxClusterStateCASRetries = 5

// PutClusterMeta writes the meta record unconditionally. Used at
// CreateCluster time to lay down the initial CREATING record.
func PutClusterMeta(kv nats.KeyValue, meta *ClusterMeta) error {
	if meta == nil {
		return errors.New("eks: PutClusterMeta nil meta")
	}
	if meta.Name == "" {
		return errors.New("eks: PutClusterMeta meta.Name empty")
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal cluster meta %s: %w", meta.Name, err)
	}
	if _, err := kv.Put(ClusterMetaKey(meta.Name), data); err != nil {
		return fmt.Errorf("kv put %s: %w", ClusterMetaKey(meta.Name), err)
	}
	return nil
}

// GetClusterMeta reads the meta record. Returns ErrClusterNotFound if the
// key is absent.
func GetClusterMeta(kv nats.KeyValue, name string) (*ClusterMeta, error) {
	if name == "" {
		return nil, errors.New("eks: GetClusterMeta empty name")
	}
	entry, err := kv.Get(ClusterMetaKey(name))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, ErrClusterNotFound
		}
		return nil, fmt.Errorf("kv get %s: %w", ClusterMetaKey(name), err)
	}
	var meta ClusterMeta
	if err := json.Unmarshal(entry.Value(), &meta); err != nil {
		return nil, fmt.Errorf("unmarshal cluster meta %s: %w", name, err)
	}
	return &meta, nil
}

// SetClusterStatus does a revision-checked update of the meta.Status field.
// Retries on KV CAS conflict (concurrent reconciler write) up to
// maxClusterStateCASRetries. Returns ErrClusterNotFound if the meta record
// was deleted underneath us.
func SetClusterStatus(kv nats.KeyValue, name string, status ClusterStatus) error {
	if name == "" {
		return errors.New("eks: SetClusterStatus empty name")
	}
	for range maxClusterStateCASRetries {
		entry, err := kv.Get(ClusterMetaKey(name))
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				return ErrClusterNotFound
			}
			return fmt.Errorf("kv get %s: %w", ClusterMetaKey(name), err)
		}
		var meta ClusterMeta
		if err := json.Unmarshal(entry.Value(), &meta); err != nil {
			return fmt.Errorf("unmarshal cluster meta %s: %w", name, err)
		}
		if meta.Status == status {
			return nil
		}
		meta.Status = status
		data, err := json.Marshal(&meta)
		if err != nil {
			return fmt.Errorf("marshal cluster meta %s: %w", name, err)
		}
		_, err = kv.Update(ClusterMetaKey(name), data, entry.Revision())
		if err == nil {
			return nil
		}
		if errors.Is(err, nats.ErrKeyExists) {
			continue
		}
		return fmt.Errorf("kv update %s: %w", ClusterMetaKey(name), err)
	}
	return fmt.Errorf("eks: SetClusterStatus %s exhausted CAS retries", name)
}

// SetClusterCertificateAuthority does a revision-checked update of the
// meta.CertificateAuthorityB64 field. The NATS bootstrap subscriber calls
// this once the K3s server VM publishes its CA on the bootstrap bus. Retries
// on KV CAS conflict (concurrent reconciler write) up to
// maxClusterStateCASRetries. Returns ErrClusterNotFound if the meta record
// was deleted underneath us.
func SetClusterCertificateAuthority(kv nats.KeyValue, name, caB64 string) error {
	if name == "" {
		return errors.New("eks: SetClusterCertificateAuthority empty name")
	}
	if caB64 == "" {
		return errors.New("eks: SetClusterCertificateAuthority empty CA")
	}
	for range maxClusterStateCASRetries {
		entry, err := kv.Get(ClusterMetaKey(name))
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				return ErrClusterNotFound
			}
			return fmt.Errorf("kv get %s: %w", ClusterMetaKey(name), err)
		}
		var meta ClusterMeta
		if err := json.Unmarshal(entry.Value(), &meta); err != nil {
			return fmt.Errorf("unmarshal cluster meta %s: %w", name, err)
		}
		if meta.CertificateAuthorityB64 == caB64 {
			return nil
		}
		meta.CertificateAuthorityB64 = caB64
		data, err := json.Marshal(&meta)
		if err != nil {
			return fmt.Errorf("marshal cluster meta %s: %w", name, err)
		}
		_, err = kv.Update(ClusterMetaKey(name), data, entry.Revision())
		if err == nil {
			return nil
		}
		if errors.Is(err, nats.ErrKeyExists) {
			continue
		}
		return fmt.Errorf("kv update %s: %w", ClusterMetaKey(name), err)
	}
	return fmt.Errorf("eks: SetClusterCertificateAuthority %s exhausted CAS retries", name)
}

// DeleteClusterPrefix removes every KV key under clusters/{name}/. Called
// from DeleteCluster after the OIDC private key has been zeroized.
// Returns the first delete error encountered but continues sweeping so
// best-effort cleanup proceeds.
func DeleteClusterPrefix(kv nats.KeyValue, name string) error {
	if name == "" {
		return errors.New("eks: DeleteClusterPrefix empty name")
	}
	prefix := fmt.Sprintf("clusters/%s/", name)
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("kv keys: %w", err)
	}
	var firstErr error
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if err := kv.Delete(k); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("kv delete %s: %w", k, err)
		}
	}
	return firstErr
}
