package dns

import (
	"context"
	"time"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/nats-io/nats.go"
)

// zoneReloadTimeout bounds one wait for every northstar server to load a zone.
const zoneReloadTimeout = 5 * time.Second

// ZonePublisher writes whole zone documents to the northstar zone store and
// asks every northstar server to load them, for callers that own a zone
// outright rather than editing records in one.
type ZonePublisher struct {
	s3      *nsconfig.S3Config
	nc      *nats.Conn
	cluster func() *config.ClusterConfig
}

// NewZonePublisher returns a publisher, or false when northstar's S3 zone store
// is not configured on this node. cluster is read on every reload, so a node
// joining changes how many acknowledgements are awaited.
func NewZonePublisher(cfg *config.Config, cluster func() *config.ClusterConfig, nc *nats.Conn) (*ZonePublisher, bool) {
	zoneCfg, ok := zoneS3Config(cfg)
	if !ok || nc == nil {
		return nil, false
	}
	return &ZonePublisher{s3: zoneCfg.s3, nc: nc, cluster: cluster}, true
}

// PutZone writes apex's zone document.
func (p *ZonePublisher) PutZone(_ context.Context, apex string, body []byte) error {
	return nsconfig.WriteZoneFile(p.s3, apex, body)
}

// DeleteZone removes apex's zone document.
func (p *ZonePublisher) DeleteZone(_ context.Context, apex string) error {
	return nsconfig.DeleteZoneFile(p.s3, apex)
}

// Reload asks every northstar server to load apex and returns the version each
// reports, "" where it serves nothing. A server that failed to load reports a
// value no digest can equal. complete is whether every server answered,
// counted by node so a repeated acknowledgement cannot stand in for another.
func (p *ZonePublisher) Reload(ctx context.Context, apex string) ([]string, bool, error) {
	want := NorthstarServerCount(p.cluster())
	ctx, cancel := context.WithTimeout(ctx, zoneReloadTimeout)
	defer cancel()
	acks, err := RequestZoneReload(ctx, p.nc, apex, want)
	if err != nil {
		return nil, false, err
	}
	served := make([]string, 0, len(acks))
	nodes := map[string]bool{}
	for _, ack := range acks {
		nodes[ack.Node] = true
		if ack.Error != "" {
			served = append(served, "error: "+ack.Error)
			continue
		}
		served = append(served, ack.ETag)
	}
	return served, len(nodes) >= want, nil
}

// NorthstarServerCount is how many northstar servers the cluster runs, and so
// how many must acknowledge a reload. A cluster with none configured still runs
// the local one.
func NorthstarServerCount(cluster *config.ClusterConfig) int {
	if cluster == nil {
		return 1
	}
	return max(1, len(configuredNorthstarNodeNames(cluster)))
}
