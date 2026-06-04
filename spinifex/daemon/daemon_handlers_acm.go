package daemon

import "github.com/nats-io/nats.go"

func (d *Daemon) handleACMImportCertificate(msg *nats.Msg) {
	handleNATSRequest(msg, d.acmService.ImportCertificate)
}

func (d *Daemon) handleACMDescribeCertificate(msg *nats.Msg) {
	handleNATSRequest(msg, d.acmService.DescribeCertificate)
}

func (d *Daemon) handleACMListCertificates(msg *nats.Msg) {
	handleNATSRequest(msg, d.acmService.ListCertificates)
}

func (d *Daemon) handleACMDeleteCertificate(msg *nats.Msg) {
	handleNATSRequest(msg, d.acmService.DeleteCertificate)
}
