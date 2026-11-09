//go:build e2e

package harness

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/foundation/netaddr"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// UEFIStoreView reads a UEFI instance's variable store the way the platform
// holds it: the store record in KV and the objects under the instance's prefix
// in the node's Predastore bucket. It only reads.
type UEFIStoreView struct {
	nc      *nats.Conn
	records *uefistore.Store
	objects objectstore.ObjectStore
	bucket  string
}

// NewUEFIStoreView builds a view from the local node's spinifex.toml, using the
// same Predastore credentials and bucket the daemon writes the store with.
// The view outlives the test that builds it, so the caller must Close it.
func NewUEFIStoreView(t *testing.T, env *Env) *UEFIStoreView {
	t.Helper()
	cfgPath := filepath.Join(env.ConfigDir, "spinifex.toml")
	cc, err := loadClusterConfig(cfgPath)
	if err != nil {
		t.Fatalf("load %s for the Predastore settings: %v", cfgPath, err)
	}
	node := nodeConfig(cc)
	if node == nil {
		t.Fatalf("no node stanza with a NATS host in %s", cfgPath)
	}
	p := node.Predastore
	if p.Host == "" || p.Bucket == "" {
		t.Fatalf("%s names no Predastore host or bucket, so the UEFI store objects cannot be read", cfgPath)
	}
	host, token, ca := natsConn(t, env)
	nc, err := natsmsg.ConnectNATS(host, token, ca)
	if err != nil {
		t.Fatalf("connect NATS %s: %v", host, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		t.Fatalf("jetstream context: %v", err)
	}
	return &UEFIStoreView{
		nc:      nc,
		records: uefistore.NewReader(js),
		objects: objectstore.NewS3ObjectStoreFromConfig(netaddr.DialTarget(p.Host), p.Region, p.AccessKey, p.SecretKey),
		bucket:  p.Bucket,
	}
}

// Close releases the view's NATS connection.
func (v *UEFIStoreView) Close() { v.nc.Close() }

// Record returns the instance's store record as the KV leader holds it.
func (v *UEFIStoreView) Record(ctx context.Context, instanceID string) (*uefistore.UEFIStoreRecord, error) {
	rec, _, err := v.records.LeaderGet(ctx, instanceID)
	return rec, err
}

// Objects returns the key of every object under the instance's store prefix.
func (v *UEFIStoreView) Objects(ctx context.Context, instanceID string) ([]string, error) {
	return uefistore.ListObjects(ctx, v.objects, v.bucket, instanceID)
}

// Region fetches the object at key and returns the VARS region it holds.
func (v *UEFIStoreView) Region(ctx context.Context, key string) ([]byte, error) {
	out, err := v.objects.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(v.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	gz, err := gzip.NewReader(out.Body)
	if err != nil {
		return nil, fmt.Errorf("%s is not gzip: %w", key, err)
	}
	var region bytes.Buffer
	if _, err := io.Copy(&region, gz); err != nil {
		return nil, fmt.Errorf("decompress %s: %w", key, err)
	}
	return region.Bytes(), nil
}
