package daemon

import (
	"net"
	"os"
	"path/filepath"

	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"log/slog"
)

// systemRoleEnsurer lazily builds — and memoizes — the KV-backed IAM service
// that find-or-creates the system instance roles/profiles backing IMDS
// instance-role credentials. It is built on first use, NOT at daemon startup:
// the NATS KV backend has no responders until JetStream is ready, so an eager
// build races boot and, on a node that loses, fails permanently with no retry,
// silently dropping every system VM it launches to baked static creds. By
// first-use (an LB/cluster create) the backend is up. Returns nil (caller falls
// back to static creds) until a build succeeds; retried on each call until then,
// cached once it works.
func (d *Daemon) systemRoleEnsurer() handlers_iam.SystemInstanceRoleEnsurer {
	d.iamEnsurerMu.Lock()
	defer d.iamEnsurerMu.Unlock()
	if d.iamEnsurerCached != nil {
		return d.iamEnsurerCached
	}
	masterKey, err := handlers_iam.LoadMasterKey(filepath.Join(filepath.Dir(d.configPath), "master.key"))
	if err != nil || masterKey == nil {
		slog.Warn("System role ensurer: master key unavailable; system VMs fall back to baked static creds",
			"err", err)
		return nil
	}
	clusterSize := 1
	if d.clusterConfig != nil {
		clusterSize = len(d.clusterConfig.Nodes)
	}
	iamSvc, iamErr := handlers_iam.NewIAMServiceImpl(d.ctx, d.natsConn, masterKey, clusterSize)
	if iamErr != nil {
		slog.Warn("System role ensurer: IAM service init failed (retried on next launch); system VMs fall back to baked static creds",
			"err", iamErr)
		return nil
	}
	d.iamEnsurerCached = iamSvc
	return iamSvc
}

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
	clusterSize := 1
	if d.clusterConfig != nil {
		internalSuffix = d.clusterConfig.AWS.InternalSuffix
		clusterSize = len(d.clusterConfig.Nodes)
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

	deps := handlers_eks.EKSServiceDeps{
		Config:              d.config,
		NATSConn:            d.natsConn,
		MasterKey:           masterKey,
		GatewayBaseURL:      d.resolveGatewayBaseURL(),
		Region:              d.config.Region,
		HolderID:            d.node,
		ClusterSize:         clusterSize,
		InternalSuffix:      internalSuffix,
		SystemGatewayURL:    d.resolveSystemGatewayBaseURL(),
		SystemAccessKey:     d.config.Predastore.AccessKey,
		SystemSecretKey:     d.config.Predastore.SecretKey,
		GatewayCACert:       gatewayCA,
		SystemPredastoreURL: d.resolveSystemPredastoreURL(),
		SnapshotStore: objectstore.NewS3ObjectStoreFromConfig(
			d.config.Predastore.Host, d.config.Predastore.Region,
			d.config.Predastore.AccessKey, d.config.Predastore.SecretKey),
		VPCSG:          d.vpcService,
		VPCK3s:         d.vpcService,
		VPCSubnet:      &daemonEKSSubnetResolver{d: d},
		NLB:            d.elbv2Service,
		Instance:       d,
		Image:          d.imageService,
		EIP:            d.eipService,
		IGW:            d.igwService,
		Worker:         d,
		VPCMgr:         d.vpcService,
		NATGW:          d.natGatewayService,
		RouteTable:     d.routeTableService,
		PlacementGroup: handlers_ec2_placementgroup.NewNATSPlacementGroupService(d.natsConn),
		Scheduler:      handlers_eks.NewNATSHostScheduler(d.natsConn),
		CPControl:      d.newEKSCPControl(),
	}

	// A KV-backed IAM service (sharing the gateway's buckets over NATS) lets EKS
	// find-or-create the node-role + CP instance profiles. Provided lazily so it
	// resolves at cluster-launch time rather than racing the NATS KV backend at
	// daemon startup; absent (no master key) workers/CP fall back accordingly.
	deps.IAMProvider = d.systemRoleEnsurer

	return deps
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

// resolveSystemPredastoreURL derives the predastore URL a system instance (the
// K3s server VM) fetches/PUTs etcd snapshots to. Same mgmt-bridge-only
// reachability constraint as resolveSystemGatewayBaseURL: config.Predastore.Host
// is rewritten to 127.0.0.1 for the local daemon, which a guest cannot dial, so
// the bridge IP is substituted with the configured port. Falls back to the
// configured Predastore.Host (scheme-prefixed) when no mgmt bridge exists.
func (d *Daemon) resolveSystemPredastoreURL() string {
	predastorePort := "8443"
	if d.config.Predastore.Host != "" {
		if _, port, splitErr := net.SplitHostPort(d.config.Predastore.Host); splitErr == nil && port != "" {
			predastorePort = port
		}
	}
	if d.mgmtBridgeIP == "" {
		if d.config.Predastore.Host == "" {
			return ""
		}
		return "https://" + d.config.Predastore.Host
	}
	return "https://" + net.JoinHostPort(d.mgmtBridgeIP, predastorePort)
}
