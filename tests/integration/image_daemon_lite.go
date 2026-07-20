//go:build integration

package integration

import (
	"testing"

	handlers_ec2_image "github.com/mulgadc/spinifex/spinifex/handlers/ec2/image"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/nats-io/nats.go"
)

// testImageBucket is the bucket name StartImageDaemonLite's ImageServiceImpl
// stores AMI config.json objects under. Backed by a MemoryObjectStore (see
// below), so — like testPredastoreBucket in daemon_lite.go — the name is
// never resolved against a real Predastore.
const testImageBucket = "integration-test-images"

// StartImageDaemonLite subscribes a real handlers_ec2_image.ImageServiceImpl
// — the same production code a live daemon runs (daemon/daemon_handlers_image.go)
// — to the ec2.DescribeImages/ec2.RegisterImage subjects, backed by a memory
// object store rather than a real Predastore. The store itself is returned
// too, so a test can seed fixture data (e.g. snapshot metadata RegisterImage
// requires) directly, without a round-trip through NATS.
//
// Deliberately separate from StartDaemonLite (like StartVolumeDaemonLite):
// only a test that actually exercises image registration/lookup should wire
// it. CreateImage/CopyImage/DeregisterImage/*ImageAttribute are not wired —
// no ported test needs them yet.
func StartImageDaemonLite(t *testing.T, gw *Gateway) (*handlers_ec2_image.ImageServiceImpl, objectstore.ObjectStore) {
	t.Helper()

	store := objectstore.NewMemoryObjectStore()
	svc := handlers_ec2_image.NewImageServiceImplWithStore(store, testImageBucket)

	nc := gw.NATSConn
	sub(t, nc, "ec2.DescribeImages", func(m *nats.Msg) { dispatch(m, svc.DescribeImages) })
	sub(t, nc, "ec2.RegisterImage", func(m *nats.Msg) { dispatch(m, svc.RegisterImage) })

	return svc, store
}
