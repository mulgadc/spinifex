//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ec2_volume "github.com/mulgadc/spinifex/spinifex/handlers/ec2/volume"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	testpredastore "github.com/mulgadc/spinifex/tests/fixtures/predastore"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// testVolumeBucket is the real predastore bucket StartVolumeDaemonLite's
// VolumeServiceImpl is wired against. Unlike testPredastoreBucket
// (memory-backed, name never resolved — see StartDaemonLite), this bucket
// genuinely exists on the shared predastore fixture: viperblock's S3 backend
// dials it for real, so chunk uploads and config.json persistence are real.
const testVolumeBucket = "integration-test-volumes"

// StartVolumeDaemonLite subscribes a real handlers_ec2_volume.VolumeServiceImpl
// — the same production code a live daemon runs (daemon/daemon_handlers_volume.go)
// — to the ec2.CreateVolume/DeleteVolume/DescribeVolumes subjects, backed by a
// real predastore daemon (testpredastore.Start) rather than the memory-backed
// stores DaemonLite's other resources use.
//
// This is deliberately a separate opt-in wiring rather than part of
// StartDaemonLite: CreateVolume constructs viperblock.New and calls
// Backend.Init() unconditionally (handlers/ec2/volume/service_impl.go), so it
// needs a reachable Predastore to do anything beyond input validation, while
// every other DaemonLite-wired resource is happy with an in-memory stand-in.
// Only a test that actually exercises volume storage should pay the shared
// predastore daemon's startup cost.
//
// Must be called before a test issues ec2.CreateVolume/DeleteVolume/
// DescribeVolumes; like StartECRDaemonLite it wires its own subjects
// independent of StartDaemonLite.
func StartVolumeDaemonLite(t *testing.T, gw *Gateway) *handlers_ec2_volume.VolumeServiceImpl {
	t.Helper()

	fixture := testpredastore.Start(t)

	store := objectstore.NewS3ObjectStoreFromConfig(fixture.Host, fixture.Region, fixture.AccessKey, fixture.SecretKey)
	require.NoError(t, store.EnsureBucket(context.Background(), testVolumeBucket), "ensure volume test bucket")

	cfg := &config.Config{
		AZ: testAZ,
		Predastore: config.PredastoreConfig{
			Host:      fixture.Host,
			Bucket:    testVolumeBucket,
			Region:    fixture.Region,
			AccessKey: fixture.AccessKey,
			SecretKey: fixture.SecretKey,
		},
		// Real WAL/chunk files land here (viperblock.VB.BaseDir); a per-test
		// dir is safe since only the predastore daemon itself is shared.
		WalDir: t.TempDir(),
	}

	nc := gw.NATSConn
	svc := handlers_ec2_volume.NewVolumeServiceImplWithStore(cfg, store, nc)

	sub(t, nc, "ec2.CreateVolume", func(m *nats.Msg) { dispatch(m, svc.CreateVolume) })
	sub(t, nc, "ec2.DeleteVolume", func(m *nats.Msg) { dispatch(m, svc.DeleteVolume) })
	sub(t, nc, "ec2.DescribeVolumes", func(m *nats.Msg) { dispatch(m, svc.DescribeVolumes) })

	return svc
}
