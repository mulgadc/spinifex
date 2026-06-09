package handlers_eks

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

// imdsServerIP is the link-local IMDS address (mirror of
// handlers/imds.MetaDataServerIP, duplicated to avoid an import cycle:
// imds → sts → eks).
const imdsServerIP = "169.254.169.254"

// ErrEKSServerAMINotFound is returned by the launcher when no AMI carrying the
// EKS managed-by tag exists in the account. CreateCluster maps it to a precise
// client error: it signals an operator/config gap (the eks-server image was
// never built/imported), not an unrecoverable internal fault.
var ErrEKSServerAMINotFound = errors.New("eks: eks-server AMI not found")

// k3sVPCProvisioner is the subset of handlers_ec2_vpc.VPCService that the K3s
// server VM launcher needs. Narrow so tests can fake it without implementing
// the full VPC surface.
type k3sVPCProvisioner interface {
	CreateNetworkInterface(input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error)
	DeleteNetworkInterface(input *ec2.DeleteNetworkInterfaceInput, accountID string) (*ec2.DeleteNetworkInterfaceOutput, error)
}

// k3sInstanceLauncher is the system-instance launch surface the K3s control-plane
// VM needs. The VM boots from the eks-server AMI (BootAMI) and must get a
// management-bridge NIC so it can reach the daemon's NATS endpoint off its
// tenant VPC subnet — which only the system-instance path provides.
// *daemon.Daemon satisfies this interface.
type k3sInstanceLauncher interface {
	LaunchSystemInstance(input *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error)
	// LaunchSystemInstanceOnNode places the VM on a specific Spinifex host for
	// HA control-plane spread. An empty nodeID (or the local node) launches
	// in-process, matching LaunchSystemInstance.
	LaunchSystemInstanceOnNode(nodeID string, input *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error)
	TerminateSystemInstance(instanceID string) error
}

// k3sAMIResolver is the subset of handlers_ec2_image.ImageService the launcher
// uses to resolve the eks-server AMI name to an AMI ID at launch time.
type k3sAMIResolver interface {
	DescribeImages(input *ec2.DescribeImagesInput, accountID string) (*ec2.DescribeImagesOutput, error)
}

const (
	// defaultK3sServerInstanceType is the internal system type the k3s
	// control-plane VM boots as (2 vCPU / 4 GB / 40 GB root). A sys.* type
	// keeps the CP VM out of customer DescribeInstanceTypes and lets the daemon
	// register the node-targeted system.LaunchInstance.{type}.{nodeID} subject
	// the HA spread path uses. Callers can override via K3sServerInput.InstanceType.
	defaultK3sServerInstanceType = "sys.medium"

	// k3sOIDCSigningKeyPath is the on-VM path where cloud-init drops the
	// per-cluster OIDC private key PEM. K3s reads it via the
	// service-account-signing-key-file kube-apiserver arg (signs SA tokens).
	k3sOIDCSigningKeyPath = "/etc/rancher/k3s/oidc-signing-key.pem"

	// k3sOIDCPublicKeyPath is where cloud-init drops the matching public key
	// PEM. kube-apiserver's service-account-key-file requires a PUBLIC key
	// (it rejects a private-key PEM with "data does not contain any valid RSA
	// or ECDSA public keys" and crash-loops), so it must be a separate file.
	k3sOIDCPublicKeyPath = "/etc/rancher/k3s/oidc-signing-key.pub.pem"

	// k3sFirstBootEnvPath is the env file k3s-first-boot.sh sources at boot
	// (see scripts/images/eks-server/k3s-first-boot.sh ENVFILE).
	k3sFirstBootEnvPath = "/etc/spinifex-eks/first-boot.env"

	// agentEnvPath is the env file the k3s-agent OpenRC service sources for its
	// K3S_URL/K3S_TOKEN/K3S_NODE_NAME/K3S_NODE_LABEL. Path matches the AGENT_ENVFILE
	// in scripts/images/eks-node/eks-node-role.sh and k3s-agent.initd.
	agentEnvPath = "/etc/spinifex-eks/agent.env"

	// k3sGatewayCAPath is the on-VM destination for the AWS gateway TLS CA cert
	// PEM. The K3s VM uses it to verify the gateway's HTTPS cert when the
	// eks-gateway-publish helper POSTs bootstrap envelopes + state reports.
	// Path matches k3s-first-boot.sh EKS_GATEWAY_CA.
	k3sGatewayCAPath = "/etc/spinifex-eks/gateway-ca.pem"

	// k3sTokenWebhookKubeconfigPath is the apiserver token-webhook config the
	// eks-token-webhook service (ordered `before k3s`) writes before the
	// apiserver starts. Wired via the authentication-token-webhook-config-file
	// apiserver arg so bearer tokens minted by `aws eks get-token` resolve
	// through the webhook. Must match the webhook's EKS_WEBHOOK_KUBECONFIG default.
	k3sTokenWebhookKubeconfigPath = "/etc/spinifex-eks/token-webhook.kubeconfig" //nolint:gosec // file path, not a credential

	// k3sConfigPath is the K3s server config file cloud-init writes; K3s
	// reads it at startup (overrides the AMI-baked config.yaml.skel).
	k3sConfigPath = "/etc/rancher/k3s/config.yaml"

	// k3sResolvConfPath is the on-VM resolver file. The Alpine eks-server AMI
	// runs dhcpcd, whose resolv.conf hook fails to persist the DHCP-supplied
	// nameservers ("can't create /etc/resolv.conf: nonexistent directory"), so
	// the VM boots with no resolver and containerd cannot resolve registry-1.
	// docker.io to pull the system-pod images. cloud-init writes a static
	// resolver here; the dhcpcd hook never clobbers it (it errors before it
	// would). Reachable via the cluster's egress SNAT.
	k3sResolvConfPath = "/etc/resolv.conf"

	// k3sResolvConf is the static resolver content. Public anycast resolvers
	// reached over the control-plane VM's egress path.
	k3sResolvConf = "nameserver 1.1.1.1\nnameserver 8.8.8.8"
)

// K3sServerInput is the launcher's input shape. AccountID is the customer
// account; the ENI + VM both live there in v1 (SystemAccount-owned VM is
// deferred behind cross-account-ENI work). Region is carried for future
// region-aware AMI lookups but not consumed today.
type K3sServerInput struct {
	AccountID        string
	ClusterName      string
	Region           string
	SubnetID         string
	ControlPlaneSGID string
	NLBDNS           string
	// EndpointIP is the reachable cluster NLB front-end IP. When set it is added
	// to the apiserver serving-cert SANs (tls-san) so kubectl reaching the
	// cluster on this IP validates TLS. Empty for an internal endpoint with no
	// front-end IP read back.
	EndpointIP        string
	OIDCIssuer        string
	OIDCPrivateKeyPEM string
	OIDCPublicKeyPEM  string
	// Gateway broker config: the control-plane VM publishes its bootstrap
	// envelopes + state reports via SigV4-signed HTTPS POST to the AWS gateway
	// (the ELBv2 lb-agent model), not by dialing core NATS. GatewayURL is the
	// mgmt-reachable AWSGW endpoint; AccessKey/SecretKey are the system
	// (Predastore) SigV4 creds; GatewayCACert is the PEM that signs the gateway
	// server cert.
	GatewayURL    string
	AccessKey     string
	SecretKey     string
	GatewayCACert string
	InstanceType  string
	// TargetNodeID pins the control-plane VM to a specific Spinifex host for HA
	// spread across distinct failure domains. Empty launches on the local
	// daemon node (the single-CP default).
	TargetNodeID string
	// JoinToken is the shared k3s cluster token (server secret). Set on every
	// control-plane server so servers 2..N authenticate into the first server's
	// embedded-etcd quorum and workers continue to register. Empty preserves
	// k3s' per-server auto-generated token (pre-HA single-CP behaviour).
	JoinToken string
	// ServerURL, when set, boots this VM as a JOIN server: it registers with the
	// existing control plane at this https://<first-server-ip>:6443 endpoint and
	// joins the etcd quorum WITHOUT cluster-init. Empty = the first server, which
	// cluster-inits the datastore. A non-empty ServerURL requires JoinToken.
	ServerURL string
	// BuiltinIngress keeps K3s' bundled traefik + servicelb enabled (interim
	// in-VPC app exposure). When false the config disables them for AWS parity,
	// where Service type=LoadBalancer / Ingress are satisfied by the AWS Load
	// Balancer Controller instead. The CreateCluster default is ON until that
	// controller ships (see managedIngressTagKey).
	BuiltinIngress bool
}

// K3sServerOutput carries the identifiers the caller needs to persist into
// ClusterMeta and to register the ENI IP with the cluster NLB target group.
type K3sServerOutput struct {
	InstanceID string
	ENIID      string
	ENIIP      string
	MgmtIP     string
}

// LaunchK3sServerVM provisions the K3s control-plane VM for an EKS cluster.
// Sequence: resolve the eks-server AMI, pre-create the customer-account ENI
// in the supplied subnet with the control-plane SG, render cloud-init user
// data (env vars, OIDC PEM, K3s config), then call RunInstances with
// NetworkInterfaces[0].NetworkInterfaceId pointing at the pre-created ENI.
// Returns instance/ENI identity so the caller can register the ENI IP with
// the cluster NLB target group and persist the IDs in ClusterMeta. On
// RunInstances failure the pre-created ENI is deleted best-effort so the
// caller does not leak a customer-account resource.
func LaunchK3sServerVM(
	vpcSvc k3sVPCProvisioner,
	instSvc k3sInstanceLauncher,
	amiSvc k3sAMIResolver,
	in K3sServerInput,
) (*K3sServerOutput, error) {
	if err := validateK3sServerInput(in); err != nil {
		return nil, err
	}
	instanceType := in.InstanceType
	if instanceType == "" {
		instanceType = defaultK3sServerInstanceType
	}

	amiID, err := lookupEKSServerAMI(amiSvc, in.AccountID)
	if err != nil {
		return nil, err
	}

	eniOut, err := vpcSvc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(in.SubnetID),
		Description: aws.String("EKS K3s server ENI for " + in.ClusterName),
		Groups:      aws.StringSlice([]string{in.ControlPlaneSGID}),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("network-interface"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
				{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(in.ClusterName)},
				{Key: aws.String(clusterEKSRoleTagKey), Value: aws.String(clusterEKSRoleControlPlane)},
			},
		}},
	}, in.AccountID)
	if err != nil {
		return nil, fmt.Errorf("create K3s ENI in subnet %s: %w", in.SubnetID, err)
	}
	if eniOut == nil || eniOut.NetworkInterface == nil ||
		aws.StringValue(eniOut.NetworkInterface.NetworkInterfaceId) == "" ||
		aws.StringValue(eniOut.NetworkInterface.PrivateIpAddress) == "" {
		return nil, errors.New("eks: CreateNetworkInterface returned incomplete ENI")
	}
	eniID := aws.StringValue(eniOut.NetworkInterface.NetworkInterfaceId)
	eniIP := aws.StringValue(eniOut.NetworkInterface.PrivateIpAddress)

	userData := buildK3sUserData(in)

	sysOut, err := instSvc.LaunchSystemInstanceOnNode(in.TargetNodeID, &sysinstance.SystemInstanceInput{
		BootMode:     sysinstance.BootAMI,
		ManagedBy:    tags.ManagedByEKS,
		InstanceType: instanceType,
		ImageID:      amiID,
		AccountID:    in.AccountID,
		ENIID:        eniID,
		ENIMac:       aws.StringValue(eniOut.NetworkInterface.MacAddress),
		ENIIP:        eniIP,
		UserData:     userData,
	})
	if err != nil {
		rollbackK3sENI(vpcSvc, in.AccountID, eniID)
		return nil, fmt.Errorf("run K3s server instance for cluster %s: %w", in.ClusterName, err)
	}
	if sysOut == nil || sysOut.InstanceID == "" {
		rollbackK3sENI(vpcSvc, in.AccountID, eniID)
		return nil, fmt.Errorf("eks: LaunchSystemInstance returned no instance for cluster %s", in.ClusterName)
	}
	instanceID := sysOut.InstanceID

	slog.Info("LaunchK3sServerVM completed",
		"clusterName", in.ClusterName,
		"accountID", in.AccountID,
		"instanceId", instanceID,
		"eniId", eniID,
		"eniIp", eniIP,
	)

	return &K3sServerOutput{
		InstanceID: instanceID,
		ENIID:      eniID,
		ENIIP:      eniIP,
		MgmtIP:     sysOut.MgmtIP,
	}, nil
}

// TerminateK3sServerVM tears down the K3s server VM for a cluster. The
// instance termination cascades ENI detach inside the customer instance path,
// then this helper deletes the ENI explicitly so DeleteCluster does not leak
// it. Missing instance/ENI is a no-op so retries stay safe.
func TerminateK3sServerVM(
	vpcSvc k3sVPCProvisioner,
	instSvc k3sInstanceLauncher,
	accountID, instanceID, eniID string,
) error {
	if instanceID == "" && eniID == "" {
		return nil
	}
	var firstErr error
	if instanceID != "" {
		// A retried DeleteCluster reaches here after the VM already drained, so
		// "instance not found" is the steady-state success case, not a failure —
		// treat it as idempotent so teardown can proceed to the ENI/SG/KV sweep.
		if err := instSvc.TerminateSystemInstance(instanceID); err != nil {
			if errors.Is(err, sysinstance.ErrSystemInstanceNotFound) {
				slog.Debug("TerminateK3sServerVM: instance already gone", "instanceId", instanceID)
			} else {
				slog.Warn("TerminateK3sServerVM: terminate failed", "instanceId", instanceID, "err", err)
				firstErr = fmt.Errorf("terminate instance %s: %w", instanceID, err)
			}
		}
	}
	if eniID != "" {
		if _, err := vpcSvc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: aws.String(eniID),
		}, accountID); err != nil {
			switch {
			case awserrors.IsErrorCode(err, awserrors.ErrorInvalidNetworkInterfaceIDNotFound),
				awserrors.IsErrorCode(err, awserrors.ErrorInvalidNetworkInterfaceNotFound):
				// The instance-terminate cascade (or a prior retry) already
				// deleted the ENI. Idempotent success — must NOT block the SG +
				// KV sweep, or the cluster wedges in DELETING permanently.
				slog.Debug("TerminateK3sServerVM: ENI already gone", "eniId", eniID)
			default:
				// InvalidNetworkInterface.InUse (the VM is still terminating
				// async and holds the ENI) and any other error are retryable:
				// surface so the cluster stays DELETING and the reconciler
				// retries once the instance releases the ENI.
				slog.Warn("TerminateK3sServerVM: ENI delete failed", "eniId", eniID, "err", err)
				if firstErr == nil {
					firstErr = fmt.Errorf("delete ENI %s: %w", eniID, err)
				}
			}
		}
	}
	return firstErr
}

// lookupEKSServerAMI resolves the EKS control-plane AMI by the
// spinifex:managed-by=eks tag the build pipeline stamps on it, rather than a
// brittle exact name. This survives the planned server+agent → single EKS AMI
// unify untouched (the unified AMI keeps the tag; role is chosen per-instance
// at launch). If more than one AMI carries the tag (e.g. server + agent both
// imported pre-unify), the newest by CreationDate wins.
func lookupEKSServerAMI(amiSvc k3sAMIResolver, accountID string) (string, error) {
	out, err := amiSvc.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:" + tags.ManagedByKey), Values: aws.StringSlice([]string{tags.ManagedByEKS})},
		},
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("describe eks AMI (tag:%s=%s): %w", tags.ManagedByKey, tags.ManagedByEKS, err)
	}

	var (
		newestID      string
		newestCreated string
		matches       int
	)
	for _, img := range out.Images {
		if img == nil || img.ImageId == nil || *img.ImageId == "" {
			continue
		}
		matches++
		// CreationDate is a fixed-width RFC3339 timestamp, so lexicographic
		// comparison orders it correctly without parsing.
		if created := aws.StringValue(img.CreationDate); newestID == "" || created > newestCreated {
			newestID, newestCreated = *img.ImageId, created
		}
	}
	if newestID == "" {
		return "", fmt.Errorf("%w (tag:%s=%s, account %s)", ErrEKSServerAMINotFound, tags.ManagedByKey, tags.ManagedByEKS, accountID)
	}
	if matches > 1 {
		slog.Warn("eks: multiple AMIs match managed-by=eks; using newest",
			"count", matches, "imageId", newestID, "created", newestCreated)
	}
	return newestID, nil
}

func rollbackK3sENI(vpcSvc k3sVPCProvisioner, accountID, eniID string) {
	if _, err := vpcSvc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniID),
	}, accountID); err != nil {
		slog.Warn("LaunchK3sServerVM: rollback ENI delete failed", "eniId", eniID, "err", err)
	}
}

func validateK3sServerInput(in K3sServerInput) error {
	switch {
	case in.AccountID == "":
		return errors.New("eks: K3sServerInput empty AccountID")
	case in.ClusterName == "":
		return errors.New("eks: K3sServerInput empty ClusterName")
	case in.SubnetID == "":
		return errors.New("eks: K3sServerInput empty SubnetID")
	case in.ControlPlaneSGID == "":
		return errors.New("eks: K3sServerInput empty ControlPlaneSGID")
	case in.NLBDNS == "":
		return errors.New("eks: K3sServerInput empty NLBDNS")
	case in.OIDCIssuer == "":
		return errors.New("eks: K3sServerInput empty OIDCIssuer")
	case strings.TrimSpace(in.OIDCPrivateKeyPEM) == "":
		return errors.New("eks: K3sServerInput empty OIDCPrivateKeyPEM")
	case strings.TrimSpace(in.OIDCPublicKeyPEM) == "":
		return errors.New("eks: K3sServerInput empty OIDCPublicKeyPEM")
	case in.GatewayURL == "":
		return errors.New("eks: K3sServerInput empty GatewayURL")
	case in.AccessKey == "":
		return errors.New("eks: K3sServerInput empty AccessKey")
	case in.SecretKey == "":
		return errors.New("eks: K3sServerInput empty SecretKey")
	case strings.TrimSpace(in.GatewayCACert) == "":
		return errors.New("eks: K3sServerInput empty GatewayCACert")
	case in.ServerURL != "" && in.JoinToken == "":
		return errors.New("eks: K3sServerInput join server (ServerURL set) requires JoinToken")
	}
	return nil
}

// GenerateK3sClusterToken mints the shared k3s cluster token (256 bits of
// crypto-random, hex-encoded) seeded into every control-plane server so servers
// 2..N join the first server's etcd quorum and workers register. Generated per
// CreateCluster; not persisted (the node-token derived from it is published on
// the bootstrap bus for workers).
func GenerateK3sClusterToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("eks: generate k3s cluster token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// k3sServerJoinURL builds the registration endpoint a join server dials: the
// first server's ENI private IP on the k3s supervisor port. Token-based join is
// trust-on-first-use, so the IP need not be in the apiserver cert SANs.
func k3sServerJoinURL(ip string) string {
	return "https://" + net.JoinHostPort(ip, "6443")
}

// boolFlag renders a bool as the "1"/"0" string the shell first-boot env reads.
func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// userDataFile is one entry in the cloud-config write_files block.
type userDataFile struct {
	Path  string
	Perms string
	Body  string
}

// buildK3sUserData renders the cloud-config YAML the K3s server VM consumes
// at first boot. Output is unencoded YAML; the caller base64-wraps it for
// the RunInstances UserData field.
func buildK3sUserData(in K3sServerInput) string {
	nlbEndpoint := "https://" + net.JoinHostPort(in.NLBDNS, strconv.FormatInt(clusterNLBListenPort, 10))

	// Role the eks-node-role first-boot selector reads to enable the
	// control-plane services. The first server is "server" (cluster-init +
	// bootstrap publisher); servers 2..N are "server-join" (register into the
	// existing quorum, no bootstrap re-publish). Nodegroup workers seed "agent"
	// through their own path.
	role := "server"
	if in.ServerURL != "" {
		role = "server-join"
	}

	envBody := strings.Join([]string{
		"SPINIFEX_K3S_ROLE=" + role,
		"EKS_GATEWAY_URL=" + in.GatewayURL,
		"EKS_GATEWAY_CA=" + k3sGatewayCAPath,
		"EKS_ACCESS_KEY=" + in.AccessKey,
		"EKS_SECRET_KEY=" + in.SecretKey,
		"EKS_REGION=" + in.Region,
		"EKS_ACCOUNT_ID=" + in.AccountID,
		"EKS_CLUSTER_NAME=" + in.ClusterName,
		"EKS_NLB_ENDPOINT=" + nlbEndpoint,
		"EKS_OIDC_ISSUER=" + in.OIDCIssuer,
		// Gate deferred built-in ingress: k3s.initd stages a traefik .skip marker
		// before boot and the state-reporter removes it once the apiserver is
		// stable. Set only for clusters that want built-in ingress.
		"EKS_DEFER_TRAEFIK=" + boolFlag(in.BuiltinIngress),
	}, "\n")

	// cluster-init (first server) selects the embedded etcd datastore (required
	// for multi-server HA); servers 2..N instead carry `server: <first>` + the
	// shared token and join the quorum without cluster-init.
	// etcd-expose-metrics surfaces etcd's own wal_fsync_duration_seconds and
	// backend_commit_duration_seconds on 127.0.0.1:2381/metrics so control-plane
	// commit latency is measurable directly, not inferred.
	//
	// anonymous-auth=true: k3s hardens it off by default, which makes the
	// apiserver answer 401 to an unauthenticated /healthz. The cluster reconciler
	// probes https://<NLB>/healthz anonymously to gate ACTIVE, so it must be
	// reachable; the default RBAC binds only the health/version non-resource
	// paths to system:unauthenticated, so this exposes nothing else.
	// In real EKS neither traefik nor servicelb exists; Service type=LoadBalancer
	// and Ingress are reconciled by the AWS Load Balancer Controller. When a
	// cluster does not opt into built-in ingress (BuiltinIngress=false) the K3s
	// built-ins are disabled outright for parity.
	//
	// When a cluster DOES want built-in ingress (dev / interim, until the AWS LB
	// controller add-on lands) traefik stays ENABLED in config so k3s writes its
	// manifest — but its helm-install Job (klipper-helm image pull + chart
	// install) hammers the embedded etcd during the fragile bootstrap window,
	// which can tip etcd fsync latency past the apiserver / leaderelection
	// deadlines and crash the control plane. So traefik is deferred, NOT disabled:
	// k3s.initd stages a deploy-controller .skip marker before the apiserver
	// starts (EKS_DEFER_TRAEFIK gates it), and the state-reporter removes that
	// marker once the apiserver is sustainedly healthy — traefik then installs
	// with etcd headroom. Disabling traefik (`disable: traefik`) is wrong for this:
	// k3s never writes traefik.yaml, leaving no manifest to un-skip. servicelb
	// (klipper) is lazy — it acts only when a LoadBalancer Service exists — so it
	// carries no bootstrap cost and stays enabled whenever built-in ingress is on.
	var configLines []string
	if in.ServerURL == "" {
		configLines = append(configLines, "cluster-init: true")
	} else {
		configLines = append(configLines, "server: "+in.ServerURL)
	}
	if in.JoinToken != "" {
		configLines = append(configLines, "token: "+in.JoinToken)
	}
	configLines = append(configLines,
		"etcd-expose-metrics: true",
		// Keep user workloads off the control plane (EKS parity — the CP is never
		// a worker). Critical here because the server node is schedulable by
		// default: a workload landing on the CP pulls its images onto the same
		// NBD/viperblock disk the embedded etcd fsyncs to, starving etcd until the
		// apiserver crashes and the VM OOMs. k3s' packaged addons tolerate
		// CriticalAddonsOnly, so they still run (here, draining to the nodegroup);
		// NoExecute also evicts anything already scheduled, not just future pods.
		"node-taint:",
		"  - CriticalAddonsOnly=true:NoExecute",
	)
	if !in.BuiltinIngress {
		configLines = append(configLines,
			"disable:",
			"  - traefik",
			"  - servicelb",
		)
	}
	configLines = append(configLines, "tls-san:", "  - "+in.NLBDNS)
	// The reachable front-end IP must be a cert SAN so kubectl reaching the
	// cluster on https://<EndpointIP>:443 validates TLS. k3s accepts IP literals
	// in tls-san and classifies them as IP SANs.
	if in.EndpointIP != "" {
		configLines = append(configLines, "  - "+in.EndpointIP)
	}
	configLines = append(configLines,
		"kube-apiserver-arg:",
		"  - service-account-key-file="+k3sOIDCPublicKeyPath,
		"  - service-account-signing-key-file="+k3sOIDCSigningKeyPath,
		"  - service-account-issuer="+in.OIDCIssuer,
		"  - api-audiences=sts.amazonaws.com",
		"  - anonymous-auth=true",
		"  - authentication-token-webhook-config-file="+k3sTokenWebhookKubeconfigPath,
		"  - authentication-token-webhook-cache-ttl=5m",
		// Pin v1 so the apiserver decodes the webhook's authentication.k8s.io/v1
		// TokenReview response; the default v1beta1 rejects the GVK mismatch (401).
		"  - authentication-token-webhook-version=v1",
	)
	k3sConfig := strings.Join(configLines, "\n")

	files := []userDataFile{
		// first-boot.env carries the system SigV4 secret key, so keep it
		// root-only (0600).
		{Path: k3sFirstBootEnvPath, Perms: "0600", Body: envBody},
		{Path: k3sOIDCSigningKeyPath, Perms: "0600", Body: strings.TrimRight(in.OIDCPrivateKeyPEM, "\n")},
		{Path: k3sOIDCPublicKeyPath, Perms: "0644", Body: strings.TrimRight(in.OIDCPublicKeyPEM, "\n")},
		{Path: k3sConfigPath, Perms: "0644", Body: k3sConfig},
		{Path: k3sGatewayCAPath, Perms: "0644", Body: strings.TrimRight(in.GatewayCACert, "\n")},
		// IMDS on-link route. Alpine's cloud-init eni renderer crashes on a
		// gateway-less route in network-config, so it's delivered out-of-band:
		// a persistent local.d script (re-applied every boot by the OpenRC
		// `local` service, enabled via runcmd below) ARPs 169.254.169.254
		// directly on the VPC subnet. This block owns the only write_files key,
		// so it must carry the route itself — appending a second write_files via
		// the generic cloud-init template would collide (last key wins, silently
		// dropping the k3s config). The device is resolved via the default route
		// (the VPC egress NIC), not a name: the persistent-net rename to vpc0
		// doesn't apply on the Alpine AMI, so the kernel name is eth0 at boot.
		{
			Path:  "/etc/local.d/imds-onlink-route.start",
			Perms: "0755",
			Body: "#!/bin/sh\n" +
				"dev=$(ip route show default | awk '{print $5; exit}')\n" +
				"[ -n \"$dev\" ] && ip route replace " + imdsServerIP + "/32 dev \"$dev\" scope link",
		},
	}

	var buf strings.Builder
	buf.WriteString("#cloud-config\n")

	// Resolver via bootcmd, NOT write_files: on the Alpine AMI /etc/resolv.conf
	// is a dangling symlink (its target dir does not exist — which is why the
	// dhcpcd hook cannot persist DHCP DNS), and pointing write_files at it makes
	// cloud-init follow the dead link, fail, and abort the WHOLE write_files
	// block (dropping first-boot.env + the k3s config). bootcmd runs before
	// write_files in the init stage and as a shell, so it can drop the symlink
	// and write a real file. Containerd needs this to resolve registry-1.docker.
	// io for system-pod image pulls; reachable over the cluster egress SNAT.
	buf.WriteString("bootcmd:\n")
	buf.WriteString("  - rm -f " + k3sResolvConfPath + "\n")
	fmt.Fprintf(&buf, "  - printf '%s\\n' > %s\n",
		strings.ReplaceAll(k3sResolvConf, "\n", "\\n"), k3sResolvConfPath)

	buf.WriteString("write_files:\n")
	for _, f := range files {
		fmt.Fprintf(&buf, "  - path: %s\n", f.Path)
		fmt.Fprintf(&buf, "    permissions: '%s'\n", f.Perms)
		buf.WriteString("    content: |\n")
		for line := range strings.SplitSeq(f.Body, "\n") {
			buf.WriteString("      ")
			buf.WriteString(line)
			buf.WriteString("\n")
		}
	}

	// Enable + start the OpenRC `local` service so the IMDS route script runs on
	// first boot and is re-applied on every subsequent boot.
	buf.WriteString("runcmd:\n")
	buf.WriteString("  - [ rc-update, add, local, default ]\n")
	buf.WriteString("  - [ rc-service, local, start ]\n")

	return buf.String()
}
