package daemon

import "github.com/nats-io/nats.go"

func (d *Daemon) handleEC2PutSpotInstanceRequests(msg *nats.Msg) {
	handleNATSRequest(msg, d.spotInstanceService.PutSpotInstanceRequests)
}

func (d *Daemon) handleEC2DescribeSpotInstanceRequests(msg *nats.Msg) {
	handleNATSRequest(msg, d.spotInstanceService.DescribeSpotInstanceRequests)
}

func (d *Daemon) handleEC2CancelSpotInstanceRequests(msg *nats.Msg) {
	handleNATSRequest(msg, d.spotInstanceService.CancelSpotInstanceRequests)
}
