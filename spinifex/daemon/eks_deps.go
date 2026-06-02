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
		NATSURL:        d.resolveSystemNATSURL(),
		NATSToken:      d.config.NATS.ACL.Token,
		NATSCACert:     natsCA,
		VPCSG:          d.vpcService,
		VPCK3s:         d.vpcService,
		VPCSubnet:      &daemonEKSSubnetResolver{d: d},
		NLB:            d.elbv2Service,
		Instance:       d,
		Image:          d.imageService,
		EIP:            d.eipService,
	}
}

// resolveGatewayHost is the single source of truth for the off-host-reachable
// address LB VMs, EKS OIDC issuers, and the EKS NATS URL all dial. startCluster
// and buildEKSServiceDeps both call it so the OIDC issuer host can never
// diverge from the host the lb-agent actually reaches (M7). Precedence:
//
//  1. br-mgmt present + AWSGW on a dedicated non-loopback IP distinct from
//     AdvertiseIP (multi-node: AWSGW on a mgmt-only IP, VPC path can't reach
//     it) → the AWSGW bind IP. startCluster additionally adds a bootcmd host
//     route via br-mgmt for this case only.
//  2. AdvertiseIP set → AdvertiseIP (single-node, or multi-node where AWSGW
//     binds the advertised IP; VMs reach it via VPC → external).
//  3. br-mgmt present + AWSGW on 0.0.0.0, no AdvertiseIP → the br-mgmt IP.
//  4. DevNetworking shim → 10.0.2.2.
//  5. AWSGW bound to a specific IP (no br-mgmt, no advertise) → that IP.
//
// Returns "" if no reachable host can be derived.
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

// resolveSystemNATSURL derives the NATS URL a system instance (the K3s server
// VM) dials to publish its one-shot bootstrap messages. Unlike a customer VM,
// a system instance reaches the daemon only over the mgmt bridge: its cloud-init
// mgmt0 NIC is on-link to mgmtBridgeIP with no off-link route, so NATS must be
// dialed at the bridge address directly. NATS listens on 0.0.0.0:4222, so the
// bridge IP is reachable. Falls back to resolveNATSURL() (gateway-host
// precedence) when no mgmt bridge exists — e.g. dev-shim networking.
func (d *Daemon) resolveSystemNATSURL() string {
	if d.mgmtBridgeIP == "" {
		return d.resolveNATSURL()
	}
	natsPort := "4222"
	if d.config.NATS.Host != "" {
		if _, port, splitErr := net.SplitHostPort(d.config.NATS.Host); splitErr == nil && port != "" {
			natsPort = port
		}
	}
	return "nats://" + net.JoinHostPort(d.mgmtBridgeIP, natsPort)
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
