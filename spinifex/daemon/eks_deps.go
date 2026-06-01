package daemon

import (
	"net"
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
// MasterKey is loaded best-effort from BaseDir/config/master.key. A missing
// key disables OIDC envelope encryption, which in turn fails CreateCluster
// at the depsReadyForOrchestration gate — the daemon logs and continues.
func (d *Daemon) buildEKSServiceDeps() handlers_eks.EKSServiceDeps {
	masterKey, err := handlers_iam.LoadMasterKey(filepath.Join(d.config.BaseDir, "config", "master.key"))
	if err != nil {
		slog.Warn("EKS: LoadMasterKey failed; CreateCluster + DeleteCluster will reject until key is provisioned",
			"err", err)
		masterKey = nil
	}

	return handlers_eks.EKSServiceDeps{
		Config:         d.config,
		NATSConn:       d.natsConn,
		MasterKey:      masterKey,
		GatewayBaseURL: d.resolveGatewayBaseURL(),
		Region:         d.config.Region,
		HolderID:       d.node,
		VPCSG:          d.vpcService,
		VPCK3s:         d.vpcService,
		VPCSubnet:      &daemonEKSSubnetResolver{d: d},
		NLB:            d.elbv2Service,
		Instance:       &daemonEKSInstanceLauncher{d: d},
		Image:          d.imageService,
	}
}

// resolveGatewayBaseURL mirrors the LB-agent gateway URL precedence that
// startCluster applies later for ELBv2 (advertise → mgmt → AWSGW bind →
// dev-shim). Centralized here so EKS OIDC issuers come from the same source
// as the LB agent target. Returns "" if no reachable host can be derived,
// in which case CreateCluster fails at depsReadyForOrchestration.
func (d *Daemon) resolveGatewayBaseURL() string {
	awsgwBindIP := ""
	if d.config.AWSGW.Host != "" {
		if h, _, splitErr := net.SplitHostPort(d.config.AWSGW.Host); splitErr == nil {
			awsgwBindIP = h
		}
	}
	advertiseIP := d.config.AdvertiseIP

	var gatewayHost string
	switch {
	case d.mgmtBridgeIP != "" && awsgwBindIP != "" && awsgwBindIP != "0.0.0.0" &&
		!net.ParseIP(awsgwBindIP).IsLoopback() && awsgwBindIP != advertiseIP:
		gatewayHost = awsgwBindIP
	case advertiseIP != "" && advertiseIP != "0.0.0.0":
		gatewayHost = advertiseIP
	case d.mgmtBridgeIP != "":
		gatewayHost = d.mgmtBridgeIP
	case d.config.Daemon.DevNetworking:
		gatewayHost = "10.0.2.2"
	case awsgwBindIP != "" && awsgwBindIP != "0.0.0.0":
		gatewayHost = awsgwBindIP
	}
	if gatewayHost == "" {
		return ""
	}

	gatewayPort := "9999"
	if d.config.AWSGW.Host != "" {
		if _, port, splitErr := net.SplitHostPort(d.config.AWSGW.Host); splitErr == nil && port != "" {
			gatewayPort = port
		}
	}
	return "https://" + net.JoinHostPort(gatewayHost, gatewayPort)
}
