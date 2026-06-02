package daemon

import (
	"net"
	"os"
	"path/filepath"

	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"log/slog"
)

// buildEKSServiceDeps assembles the EKSServiceDeps the EKS service needs to
// run CreateCluster/DescribeCluster/ListClusters/DeleteCluster end-to-end.
// All collaborators must already be initialized; callers invoke this from
// Daemon.startCluster after VPC, ELBv2, Instance, and Image services are
// ready.
//
// MasterKey is loaded best-effort from the config dir (the directory holding
// spinifex.toml — same place admin init writes master.key, and the same
// resolution daemon TLS + predastore config use). A missing key disables OIDC
// envelope encryption, which in turn fails CreateCluster at the
// depsReadyForOrchestration gate — the daemon logs and continues.
func (d *Daemon) buildEKSServiceDeps() handlers_eks.EKSServiceDeps {
	masterKey, err := handlers_iam.LoadMasterKey(filepath.Join(filepath.Dir(d.configPath), "master.key"))
	if err != nil {
		slog.Warn("EKS: LoadMasterKey failed; CreateCluster + DeleteCluster will reject until key is provisioned",
			"err", err)
		masterKey = nil
	}

	// CA PEM the K3s server VM uses to verify the daemon's NATS TLS. Same CA
	// that signs the AWSGW server cert; read here so the VM can publish its
	// one-shot bootstrap messages over the token+TLS NATS endpoint.
	natsCA := ""
	if d.config.NATS.CACert != "" {
		if caBytes, readErr := os.ReadFile(d.config.NATS.CACert); readErr == nil {
			natsCA = string(caBytes)
		} else {
			slog.Warn("EKS: read NATS CACert failed; K3s server VM will not reach NATS over TLS",
				"path", d.config.NATS.CACert, "err", readErr)
		}
	}

	return handlers_eks.EKSServiceDeps{
		Config:         d.config,
		NATSConn:       d.natsConn,
		MasterKey:      masterKey,
		GatewayBaseURL: d.resolveGatewayBaseURL(),
		Region:         d.config.Region,
		HolderID:       d.node,
		NATSURL:        d.resolveNATSURL(),
		NATSToken:      d.config.NATS.ACL.Token,
		NATSCACert:     natsCA,
		VPCSG:          d.vpcService,
		VPCK3s:         d.vpcService,
		VPCSubnet:      &daemonEKSSubnetResolver{d: d},
		NLB:            d.elbv2Service,
		Instance:       &daemonEKSInstanceLauncher{d: d},
		Image:          d.imageService,
	}
}

// resolveGatewayHost mirrors the LB-agent host precedence that startCluster
// applies later for ELBv2 (advertise → mgmt → AWSGW bind → dev-shim).
// Centralized so EKS OIDC issuers and the EKS NATS URL come from the same
// off-host-reachable source as the LB agent target. Returns "" if no
// reachable host can be derived.
func (d *Daemon) resolveGatewayHost() string {
	awsgwBindIP := ""
	if d.config.AWSGW.Host != "" {
		if h, _, splitErr := net.SplitHostPort(d.config.AWSGW.Host); splitErr == nil {
			awsgwBindIP = h
		}
	}
	advertiseIP := d.config.AdvertiseIP

	switch {
	case d.mgmtBridgeIP != "" && awsgwBindIP != "" && awsgwBindIP != "0.0.0.0" &&
		!net.ParseIP(awsgwBindIP).IsLoopback() && awsgwBindIP != advertiseIP:
		return awsgwBindIP
	case advertiseIP != "" && advertiseIP != "0.0.0.0":
		return advertiseIP
	case d.mgmtBridgeIP != "":
		return d.mgmtBridgeIP
	case d.config.Daemon.DevNetworking:
		return "10.0.2.2"
	case awsgwBindIP != "" && awsgwBindIP != "0.0.0.0":
		return awsgwBindIP
	}
	return ""
}

// resolveGatewayBaseURL is the HTTPS AWSGW endpoint EKS OIDC issuers are built
// from. Returns "" if no reachable host can be derived, in which case
// CreateCluster fails at depsReadyForOrchestration.
func (d *Daemon) resolveGatewayBaseURL() string {
	host := d.resolveGatewayHost()
	if host == "" {
		return ""
	}
	gatewayPort := "9999"
	if d.config.AWSGW.Host != "" {
		if _, port, splitErr := net.SplitHostPort(d.config.AWSGW.Host); splitErr == nil && port != "" {
			gatewayPort = port
		}
	}
	return "https://" + net.JoinHostPort(host, gatewayPort)
}

// resolveNATSURL derives the NATS URL the K3s server VM dials to publish its
// one-shot bootstrap messages. Host reuses the gateway host resolution; port
// comes from the configured NATS listen address (default 4222). Returns "" if
// no reachable host can be derived.
func (d *Daemon) resolveNATSURL() string {
	host := d.resolveGatewayHost()
	if host == "" {
		return ""
	}
	natsPort := "4222"
	if d.config.NATS.Host != "" {
		if _, port, splitErr := net.SplitHostPort(d.config.NATS.Host); splitErr == nil && port != "" {
			natsPort = port
		}
	}
	return "nats://" + net.JoinHostPort(host, natsPort)
}
