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

// ErrEKSServerAMINotFound is returned when no AMI with the EKS managed-by tag
// exists. Signals an operator/config gap (image not built/imported).
var ErrEKSServerAMINotFound = errors.New("eks: eks-server AMI not found")

// k3sVPCProvisioner is the narrow VPC surface the K3s server VM launcher needs.
type k3sVPCProvisioner interface {
	CreateNetworkInterface(input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error)
	DeleteNetworkInterface(input *ec2.DeleteNetworkInterfaceInput, accountID string) (*ec2.DeleteNetworkInterfaceOutput, error)
	DescribeNetworkInterfaces(input *ec2.DescribeNetworkInterfacesInput, accountID string) (*ec2.DescribeNetworkInterfacesOutput, error)
	DetachENI(accountID, eniID string) error
}

// k3sInstanceLauncher is the system-instance launch surface for the K3s CP VM.
// The VM boots from the eks-server AMI and needs a mgmt-bridge NIC via the
// system-instance path to reach the daemon's NATS endpoint.
type k3sInstanceLauncher interface {
	LaunchSystemInstance(input *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error)
	// LaunchSystemInstanceOnNode pins the VM to a specific host for HA spread.
	// An empty nodeID launches in-process like LaunchSystemInstance.
	LaunchSystemInstanceOnNode(nodeID string, input *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error)
	TerminateSystemInstance(instanceID string) error
}

// k3sAMIResolver is the narrow AMI surface for resolving the eks-server AMI ID.
type k3sAMIResolver interface {
	DescribeImages(input *ec2.DescribeImagesInput, accountID string) (*ec2.DescribeImagesOutput, error)
}

const (
	// defaultK3sServerInstanceType is the sys.* type for the CP VM; keeps it out of
	// customer DescribeInstanceTypes and enables node-targeted HA launch subjects.
	defaultK3sServerInstanceType = "sys.medium"

	// k3sOIDCSigningKeyPath is the on-VM OIDC private key PEM path.
	k3sOIDCSigningKeyPath = "/etc/rancher/k3s/oidc-signing-key.pem"

	// k3sOIDCPublicKeyPath is the matching public key PEM; kube-apiserver's
	// --service-account-key-file requires a PUBLIC key, so it must be separate.
	k3sOIDCPublicKeyPath = "/etc/rancher/k3s/oidc-signing-key.pub.pem"

	// k3sFirstBootEnvPath is the env file k3s-first-boot.sh sources at boot.
	k3sFirstBootEnvPath = "/etc/spinifex-eks/first-boot.env"

	// agentEnvPath is the env file the k3s-agent OpenRC service sources for
	// K3S_URL/K3S_TOKEN/K3S_NODE_NAME/K3S_NODE_LABEL.
	agentEnvPath = "/etc/spinifex-eks/agent.env"

	// k3sGatewayCAPath is the on-VM gateway TLS CA cert PEM path.
	k3sGatewayCAPath = "/etc/spinifex-eks/gateway-ca.pem"

	// k3sTokenWebhookKubeconfigPath is the apiserver token-webhook kubeconfig
	// written before k3s starts, wired via --authentication-token-webhook-config-file.
	k3sTokenWebhookKubeconfigPath = "/etc/spinifex-eks/token-webhook.kubeconfig" //nolint:gosec // file path, not a credential

	// k3sConfigPath is the K3s server config file written by cloud-init.
	k3sConfigPath = "/etc/rancher/k3s/config.yaml"

	// k3sResolvConfPath is the on-VM resolver path. The Alpine AMI's dhcpcd hook
	// cannot create /etc/resolv.conf, so cloud-init writes a static resolver here.
	k3sResolvConfPath = "/etc/resolv.conf"

	// k3sResolvConf is the static resolver; reached via the cluster's egress SNAT.
	k3sResolvConf = "nameserver 1.1.1.1\nnameserver 8.8.8.8"
)

// K3sServerInput is the launcher's input shape. AccountID is the INFRA account
// the ENI + VM are created under: the system account, since the control plane
// lives in the managed CP VPC (Set B). ClusterAccountID is the cluster-OWNER
// (customer) account that owns the cluster meta and namespaces its mgmt-plane
// identity — the VM bakes it into EKS_ACCOUNT_ID so its bootstrap publish, state
// report, and add-on fetch reach the customer cluster, not the system account.
// Region is carried for future region-aware AMI lookups but not consumed today.
type K3sServerInput struct {
	AccountID        string
	ClusterAccountID string
	ClusterName      string
	Region           string
	SubnetID         string
	// VpcID is the cluster VPC; surfaced as EKS_VPC_ID so the in-cluster LB
	// controller can pass --aws-vpc-id to the gateway elbv2/ec2 handlers.
	VpcID string
	// ELBSubnetIDs is the cluster's ELB-eligible subnets, deduped to one per AZ.
	// Surfaced as EKS_ELB_SUBNET_IDS and injected into the alb IngressClassParams
	// so every Ingress takes LBC's explicit-subnet path (the only path that honors
	// the ALBSingleSubnet gate); tag auto-discovery never threads that gate, so a
	// single-AZ cluster would otherwise dedup to 1<2 subnets and fail reconcile.
	ELBSubnetIDs     []string
	ControlPlaneSGID string
	NLBDNS           string
	// EndpointIP is the NLB front-end IP added to the apiserver cert SANs for TLS.
	// Empty for an internal endpoint with no front-end IP.
	EndpointIP string
	// PrivateEndpointIP is the customer-VPC (Set A) private-endpoint IP added to the
	// apiserver cert SANs so in-VPC clients validate TLS via https://<ip>:443.
	// Empty when private access is off.
	PrivateEndpointIP string
	OIDCIssuer        string
	OIDCPrivateKeyPEM string
	OIDCPublicKeyPEM  string
	// Gateway broker config: CP VM publishes via SigV4-signed HTTPS POST to AWSGW.
	// GatewayURL is the mgmt-reachable endpoint; AccessKey/SecretKey are system
	// SigV4 creds; GatewayCACert signs the gateway TLS cert.
	GatewayURL string
	// AddonGatewayURL is the customer-facing gateway endpoint baked into managed
	// addon pod specs (EKS_ADDON_GATEWAY_URL). Those pods run on workers, which
	// cannot reach the mgmt GatewayURL, so they target this public address.
	AddonGatewayURL string
	AccessKey       string
	SecretKey       string
	GatewayCACert   string
	InstanceType    string
	// TargetNodeID pins the VM to a specific host for HA spread; empty = local node.
	TargetNodeID string
	// JoinToken is the shared k3s cluster token so HA servers join the etcd quorum.
	// Empty = k3s auto-generated token (single-CP).
	JoinToken string
	// ServerURL boots this VM as a JOIN server joining the quorum at the given endpoint.
	// Empty = first server (cluster-init). Non-empty requires JoinToken.
	ServerURL string
}

// K3sServerOutput carries identifiers to persist in ClusterMeta and register with the NLB.
type K3sServerOutput struct {
	InstanceID string
	ENIID      string
	ENIIP      string
	MgmtIP     string
}

// LaunchK3sServerVM provisions the K3s CP VM: resolves the AMI, pre-creates the
// ENI, renders cloud-init user-data, then launches via RunInstances. On failure
// the ENI is deleted best-effort to avoid leaking a customer-account resource.
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
				{Key: aws.String(clusterEKSAccountTagKey), Value: aws.String(in.ClusterAccountID)},
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
		SubnetID:     in.SubnetID,
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

// TerminateK3sServerVM terminates the K3s server VM and deletes the ENI.
// Missing instance/ENI is a no-op for idempotent retries.
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
		// "instance not found" on a retry is idempotent success; proceed to the ENI/SG/KV sweep.
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
		if err := detachAndDeleteServerENI(vpcSvc, accountID, eniID); err != nil {
			slog.Warn("TerminateK3sServerVM: ENI delete failed", "eniId", eniID, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("delete ENI %s: %w", eniID, err)
			}
		}
	}
	return firstErr
}

// detachAndDeleteServerENI removes the server VM's control-plane ENI. Teardown
// owns this ENI and has already terminated its VM, so the attachment is
// authoritatively dead even if the record still shows it in-use — a state a
// plain force=false delete would reject as InvalidNetworkInterface.InUse
// forever, wedging EKSDeletingReaper in a no-progress loop (mulga-siv-407).
// Detach first to clear the stale attachment fields, then delete. Both calls
// tolerate an already-gone ENI (NotFound), so a race with the async
// instance-terminate cascade that removes the same ENI resolves to idempotent
// success either way.
func detachAndDeleteServerENI(vpcSvc k3sVPCProvisioner, accountID, eniID string) error {
	if err := vpcSvc.DetachENI(accountID, eniID); err != nil && !isENINotFound(err) {
		// Non-fatal: the delete below still runs and surfaces any real failure.
		slog.Debug("TerminateK3sServerVM: ENI detach failed; deleting anyway", "eniId", eniID, "err", err)
	}
	_, err := vpcSvc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniID),
	}, accountID)
	if err == nil || isENINotFound(err) {
		return nil
	}
	return err
}

// isENINotFound reports whether err is one of the ENI-absent error codes, which
// teardown treats as idempotent success.
func isENINotFound(err error) bool {
	return awserrors.IsErrorCode(err, awserrors.ErrorInvalidNetworkInterfaceIDNotFound) ||
		awserrors.IsErrorCode(err, awserrors.ErrorInvalidNetworkInterfaceNotFound)
}

// lookupEKSServerAMI resolves the EKS CP AMI by the spinifex:managed-by=eks tag
// rather than a brittle exact name. If multiple AMIs match, the newest wins.
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
	}, accountID); err != nil && !awserrors.IsNotFound(err) {
		slog.Warn("LaunchK3sServerVM: rollback ENI delete failed", "eniId", eniID, "err", err)
	}
}

func validateK3sServerInput(in K3sServerInput) error {
	switch {
	case in.AccountID == "":
		return errors.New("eks: K3sServerInput empty AccountID")
	case in.ClusterAccountID == "":
		return errors.New("eks: K3sServerInput empty ClusterAccountID")
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
	case in.AddonGatewayURL == "":
		return errors.New("eks: K3sServerInput empty AddonGatewayURL")
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

// GenerateK3sClusterToken mints the shared k3s cluster token (256 bits, hex-encoded).
// Servers 2..N and workers use it to join; the derived node-token is published on the bootstrap bus.
func GenerateK3sClusterToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("eks: generate k3s cluster token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// k3sServerJoinURL builds the join endpoint (first server's ENI IP on port 6443).
func k3sServerJoinURL(ip string) string {
	return "https://" + net.JoinHostPort(ip, "6443")
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

	// "server" = first CP (cluster-init + bootstrap publisher); "server-join" = HA join server.
	role := "server"
	if in.ServerURL != "" {
		role = "server-join"
	}

	envBody := strings.Join([]string{
		"SPINIFEX_K3S_ROLE=" + role,
		"EKS_GATEWAY_URL=" + in.GatewayURL,
		"EKS_ADDON_GATEWAY_URL=" + in.AddonGatewayURL,
		"EKS_GATEWAY_CA=" + k3sGatewayCAPath,
		"EKS_ACCESS_KEY=" + in.AccessKey,
		"EKS_SECRET_KEY=" + in.SecretKey,
		"EKS_REGION=" + in.Region,
		"EKS_VPC_ID=" + in.VpcID,
		"EKS_ELB_SUBNET_IDS=" + strings.Join(in.ELBSubnetIDs, ","),
		"EKS_ACCOUNT_ID=" + in.ClusterAccountID,
		"EKS_CLUSTER_NAME=" + in.ClusterName,
		"EKS_NLB_ENDPOINT=" + nlbEndpoint,
		"EKS_OIDC_ISSUER=" + in.OIDCIssuer,
	}, "\n")

	// First server uses cluster-init (embedded etcd); join servers set `server: <first>` + token.
	// etcd-expose-metrics: surfaces etcd fsync/commit latency on 127.0.0.1:2381/metrics.
	// anonymous-auth=false (CIS): cluster health rides the authenticated NATS
	// state-report (probes /healthz via the node's admin kubeconfig), not an
	// unauthenticated apiserver probe; the NLB target group uses a TCP health check.
	// traefik+servicelb+local-storage are always disabled for AWS LB Controller parity.
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
		// Managed-CP: the apiserver VM sits in the system CP VPC with no route to
		// the worker pod network. `cluster` routes apiserver->pod/service traffic
		// (admission webhooks, aggregated APIs) through the agent's outbound tunnel;
		// the default `agent` mode tunnels only kubelet, leaving webhooks unreachable.
		"egress-selector-mode: cluster",
		// Prevent user workloads on the CP (EKS parity). k3s packaged addons tolerate CriticalAddonsOnly.
		"node-taint:",
		"  - CriticalAddonsOnly=true:NoExecute",
	)
	configLines = append(configLines,
		"disable:",
		"  - traefik",
		"  - servicelb",
		// EKS has no local-path provisioner; leaving it enabled gives the
		// cluster a second default StorageClass that races ebs-gp3. Disable
		// it so the EBS CSI StorageClass is the sole default.
		"  - local-storage",
	)
	configLines = append(configLines, "tls-san:", "  - "+in.NLBDNS)
	// EndpointIP must be a cert SAN for TLS validation via https://<EndpointIP>:443.
	if in.EndpointIP != "" {
		configLines = append(configLines, "  - "+in.EndpointIP)
	}
	// The Set A private-endpoint IP is what in-VPC clients connect to; SAN it too.
	if in.PrivateEndpointIP != "" && in.PrivateEndpointIP != in.EndpointIP {
		configLines = append(configLines, "  - "+in.PrivateEndpointIP)
	}
	// advertise-address lands in the in-cluster `kubernetes` Endpoints. The default
	// CP node-ip sits in the unpeered managed-CP VPC, unreachable from worker pods;
	// advertise the NLB front-end (Set A private-endpoint, else public) — both SANed.
	advertiseIP := in.PrivateEndpointIP
	if advertiseIP == "" {
		advertiseIP = in.EndpointIP
	}
	if advertiseIP != "" {
		configLines = append(configLines, "advertise-address: "+advertiseIP)
	}
	configLines = append(configLines,
		"kube-apiserver-arg:",
		"  - service-account-key-file="+k3sOIDCPublicKeyPath,
		"  - service-account-signing-key-file="+k3sOIDCSigningKeyPath,
		"  - service-account-issuer="+in.OIDCIssuer,
		"  - api-audiences=sts.amazonaws.com",
		"  - anonymous-auth=false",
		"  - authentication-token-webhook-config-file="+k3sTokenWebhookKubeconfigPath,
		"  - authentication-token-webhook-cache-ttl=5m",
		// v1: default v1beta1 rejects authentication.k8s.io/v1 TokenReview response (401).
		"  - authentication-token-webhook-version=v1",
	)
	k3sConfig := strings.Join(configLines, "\n")

	files := []userDataFile{
		// 0600: contains system SigV4 secret key.
		{Path: k3sFirstBootEnvPath, Perms: "0600", Body: envBody},
		{Path: k3sOIDCSigningKeyPath, Perms: "0600", Body: strings.TrimRight(in.OIDCPrivateKeyPEM, "\n")},
		{Path: k3sOIDCPublicKeyPath, Perms: "0644", Body: strings.TrimRight(in.OIDCPublicKeyPEM, "\n")},
		{Path: k3sConfigPath, Perms: "0644", Body: k3sConfig},
		{Path: k3sGatewayCAPath, Perms: "0644", Body: strings.TrimRight(in.GatewayCACert, "\n")},
	}

	var buf strings.Builder
	buf.WriteString("#cloud-config\n")

	// bootcmd (not write_files): /etc/resolv.conf is a dangling symlink on Alpine; write_files
	// follows it, fails, and aborts the entire block. bootcmd drops the symlink first.
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

	return buf.String()
}
