package daemon

import "github.com/nats-io/nats.go"

// --- Hosted zones (Sprint 1b bodies) ---

func (d *Daemon) handleRoute53CreateHostedZone(msg *nats.Msg) {
	handleNATSRequest(msg, d.route53Service.CreateHostedZone)
}

func (d *Daemon) handleRoute53GetHostedZone(msg *nats.Msg) {
	handleNATSRequest(msg, d.route53Service.GetHostedZone)
}

func (d *Daemon) handleRoute53ListHostedZones(msg *nats.Msg) {
	handleNATSRequest(msg, d.route53Service.ListHostedZones)
}

func (d *Daemon) handleRoute53UpdateHostedZoneComment(msg *nats.Msg) {
	handleNATSRequest(msg, d.route53Service.UpdateHostedZoneComment)
}

func (d *Daemon) handleRoute53DeleteHostedZone(msg *nats.Msg) {
	handleNATSRequest(msg, d.route53Service.DeleteHostedZone)
}

// --- Resource record sets + GetChange (Sprint 1c bodies) ---

func (d *Daemon) handleRoute53ChangeResourceRecordSets(msg *nats.Msg) {
	handleNATSRequest(msg, d.route53Service.ChangeResourceRecordSets)
}

func (d *Daemon) handleRoute53ListResourceRecordSets(msg *nats.Msg) {
	handleNATSRequest(msg, d.route53Service.ListResourceRecordSets)
}

func (d *Daemon) handleRoute53GetChange(msg *nats.Msg) {
	handleNATSRequest(msg, d.route53Service.GetChange)
}
