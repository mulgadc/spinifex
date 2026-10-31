package daemon

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storevolume"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// unusedEBS satisfies the provider without being called: the host check
// refuses before any volume work.
type unusedEBS struct {
	ebsprovider.EBSProvider
}

// The composed launcher's Host is the daemon's live check: with no image it
// refuses before any record, and it passes once the image is installed.
func TestBuildSQSWorkerLauncher_WiresTheRealHostCheck(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	vpc, err := ec2vpc.NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	d := &Daemon{
		node:          ensureNode,
		natsConn:      nc,
		volumeService: &ec2volume.VolumeServiceImpl{},
		ebsProvider:   unusedEBS{},
		vpcService:    vpc,
		config:        &config.Config{Region: sqsTestRegion},
	}
	ensureHost(t, d, false)

	l, err := d.buildSQSWorkerLauncher(t.Context())
	require.NoError(t, err)
	_, err = l.Ensure(t.Context(), sqsTestRegion, ensureType)
	require.ErrorIs(t, err, workerlaunch.ErrHostNotReady)

	ids, err := workerident.Open(t.Context(), js)
	require.NoError(t, err)
	_, err = ids.Current(t.Context(), sqsTestRegion)
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound, "a refused host recorded an intent")

	installWorkerImage(t)
	require.NoError(t, l.Host.Ready(t.Context()))

	// The preparer's retained volume reaches the operator, wrapped as the launcher wraps it.
	require.NotNil(t, l.RetainedVolume)
	kept := fmt.Errorf("%w: %w", workerlaunch.ErrVolumeOutcomeUnknown,
		&storevolume.RetainedVolumeError{VolumeID: ensureVolume, Err: errors.New("fill failed")})
	require.Equal(t, ensureVolume, l.RetainedVolume(kept))
}
