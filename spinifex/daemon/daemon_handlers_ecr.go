package daemon

import "github.com/nats-io/nats.go"

func (d *Daemon) handleECRRepoCreate(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.RepoCreate)
}

func (d *Daemon) handleECRRepoDescribe(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.RepoDescribe)
}

func (d *Daemon) handleECRRepoList(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.RepoList)
}

func (d *Daemon) handleECRRepoDelete(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.RepoDelete)
}

func (d *Daemon) handleECRPolicyPut(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.PolicyPut)
}

func (d *Daemon) handleECRPolicyGet(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.PolicyGet)
}

func (d *Daemon) handleECRPolicyDelete(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.PolicyDelete)
}

func (d *Daemon) handleECRTagPut(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.TagPut)
}

func (d *Daemon) handleECRTagGet(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.TagGet)
}

func (d *Daemon) handleECRTagList(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.TagList)
}

func (d *Daemon) handleECRTagDelete(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.TagDelete)
}

func (d *Daemon) handleECRManifestPut(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.ManifestPut)
}

func (d *Daemon) handleECRManifestDescribe(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.ManifestDescribe)
}

func (d *Daemon) handleECRManifestList(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.ManifestList)
}

func (d *Daemon) handleECRUploadCreate(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.UploadCreate)
}

func (d *Daemon) handleECRUploadGet(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.UploadGet)
}

func (d *Daemon) handleECRUploadUpdate(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.UploadUpdate)
}

func (d *Daemon) handleECRUploadDelete(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecrMetaService.UploadDelete)
}
