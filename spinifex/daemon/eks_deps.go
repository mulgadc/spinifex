package daemon

import (
	"net"
	"os"
	"path/filepath"

	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"log/slog"
)

// buildEKSServiceDeps assembles the EKSServiceDeps. All collaborators must
// already be initialised. MasterKey is loaded best-effort from the config dir.
func (d *Daemon) buildEKSServiceDeps() handlers_eks.EKSServiceDeps {
	masterKey, err := handlers_iam.LoadMasterKey(filepath.Join(filepath.Dir(d.configPath), "master.key"))
	if err != nil {
		slog.Warn("EKS: LoadMasterKey failed; CreateCluster + DeleteCluster will reject until key is provisioned",
			"err", err)
		masterKey = nil
	}

	internalSuffix := ""
	if d.clusterConfig != nil {
		internalSuffix = d.clusterConfig.AWS.InternalSuffix
	}

	gatewayCA := ""
	if d.config.NATS.CACert != "" {
		if caBytes, readErr := os.ReadFile(d.config.NATS.CACert); readErr == nil {
			gatewayCA = string(caBytes)
		} else {
			slog.Warn("EKS: read gateway CACert failed; K3s server VM will not verify the gateway over TLS",
				"path", d.config.NATS.CACert, "err", readErr)
		}
	}

	return handlers_eks.EKSServiceDeps{
		Config:           d.config,
		NATSConn:         d.natsConn,
		MasterKey:        masterKey,
		GatewayBaseURL:   d.resolveGatewayBaseURL(),
		Region:           d.config.Region,
		HolderID:         d.node,
		InternalSuffix:   internalSuffix,
		SystemGatewayURL: d.resolveSystemGatewayBaseURL(),
		SystemAccessKey:  d.config.Predastore.AccessKey,
		SystemSecretKey:  d.config.Predastore.SecretKey,
		GatewayCACert:    gatewayCA,
		VPCSG:            d.vpcService,
		VPCK3s:           d.vpcService,
		VPCSubnet:        &daemonEKSSubnetResolver{d: d},
		NLB:              d.elbv2Service,
		Instance:         d,
		Image:            d.imageService,
		EIP:              d.eipService,
		IGW:              d.igwService,
		Worker:           d,
		VPCMgr:           d.vpcService,
		NATGW:            d.natGatewayService,
		RouteTable:       d.routeTableService,
		PlacementGroup:   handlers_ec2_placementgroup.NewNATSPlacementGroupService(d.natsConn),
		Scheduler:        handlers_eks.NewNATSHostScheduler(d.natsConn),
	}
}

// resolveGatewayHost returns the single AWSGW host used by LB VMs, EKS OIDC
// issuers, and the EKS NATS URL. Precedence: mgmt-dedicated AWSGW IP →
// AdvertiseIP → br-mgmt IP → DevNetworking → AWSGW bind IP. Returns "".
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

// resolveGatewayBaseURL returns the HTTPS AWSGW base URL for EKS OIDC.
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

// resolveSystemGatewayBaseURL derives the AWS gateway URL a system instance
// (the K3s server VM) POSTs its bootstrap envelopes + state reports to. Unlike
// a customer LB VM — which reaches the gateway via VPC→external (AdvertiseIP) —
// a system instance reaches the daemon only over the mgmt bridge: its cloud-init
// mgmt0 NIC is on-link to mgmtBridgeIP with no off-link route, so the gateway
// must be dialed at the bridge address directly. AWSGW binds 0.0.0.0 (or the
// mgmt IP), and the server cert SANs include the bridge IP, so HTTPS validates.
// Falls back to resolveGatewayBaseURL() (gateway-host precedence) when no mgmt
// bridge exists — e.g. dev-shim networking.
func (d *Daemon) resolveSystemGatewayBaseURL() string {
	if d.mgmtBridgeIP == "" {
		return d.resolveGatewayBaseURL()
	}
	gatewayPort := "9999"
	if d.config.AWSGW.Host != "" {
		if _, port, splitErr := net.SplitHostPort(d.config.AWSGW.Host); splitErr == nil && port != "" {
			gatewayPort = port
		}
	}
	return "https://" + net.JoinHostPort(d.mgmtBridgeIP, gatewayPort)
}
