package daemon

import "github.com/nats-io/nats.go"

func (d *Daemon) handleEC2CreateLaunchTemplate(msg *nats.Msg) {
	handleNATSRequest(msg, d.launchTemplateService.CreateLaunchTemplate)
}

func (d *Daemon) handleEC2CreateLaunchTemplateVersion(msg *nats.Msg) {
	handleNATSRequest(msg, d.launchTemplateService.CreateLaunchTemplateVersion)
}

func (d *Daemon) handleEC2DeleteLaunchTemplate(msg *nats.Msg) {
	handleNATSRequest(msg, d.launchTemplateService.DeleteLaunchTemplate)
}

func (d *Daemon) handleEC2DeleteLaunchTemplateVersions(msg *nats.Msg) {
	handleNATSRequest(msg, d.launchTemplateService.DeleteLaunchTemplateVersions)
}

func (d *Daemon) handleEC2ModifyLaunchTemplate(msg *nats.Msg) {
	handleNATSRequest(msg, d.launchTemplateService.ModifyLaunchTemplate)
}

func (d *Daemon) handleEC2DescribeLaunchTemplates(msg *nats.Msg) {
	handleNATSRequest(msg, d.launchTemplateService.DescribeLaunchTemplates)
}

func (d *Daemon) handleEC2DescribeLaunchTemplateVersions(msg *nats.Msg) {
	handleNATSRequest(msg, d.launchTemplateService.DescribeLaunchTemplateVersions)
}
