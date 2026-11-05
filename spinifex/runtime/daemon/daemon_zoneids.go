package daemon

import (
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/zoneid"
	"github.com/nats-io/nats.go/jetstream"
)

// startZoneIDBackfill hands the VPC service its resolver, then pins this node's zone and every zone already named by a
// subnet record, off the request path, so an ID exists before anything reads it.
func (d *Daemon) startZoneIDBackfill() {
	if d.config.Region == "" || d.vpcService == nil {
		return
	}
	js, err := jetstream.New(d.natsConn)
	if err != nil {
		slog.Error("zone ids: jetstream unavailable, backfill skipped", "err", err)
		return
	}
	resolver, err := zoneid.NewResolver(js, d.config.Region, d.config.AZShortCode)
	if err != nil {
		slog.Error("zone ids: resolver not created, backfill skipped", "err", err)
		return
	}
	d.vpcService.SetZoneIDSource(resolver)
	go func() {
		others, err := d.vpcService.SubnetAvailabilityZones(d.ctx)
		if err != nil {
			slog.Error("zone ids: listing subnet zones failed, backfill skipped", "err", err)
			return
		}
		if err := resolver.Backfill(d.ctx, d.config.AZ, others); err != nil {
			slog.Error("zone ids: backfill failed", "region", d.config.Region, "err", err)
		}
	}()
}
