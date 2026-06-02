package handlers_eks

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

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
	TerminateSystemInstance(instanceID string) error
}

// k3sAMIResolver is the subset of handlers_ec2_image.ImageService the launcher
// uses to resolve the eks-server AMI name to an AMI ID at launch time.
type k3sAMIResolver interface {
	DescribeImages(input *ec2.DescribeImagesInput, accountID string) (*ec2.DescribeImagesOutput, error)
}

const (
	// defaultK3sServerInstanceType is the spinifex instance type closest to
	// the AWS EKS minimum control-plane footprint (2 vCPU / 8 GB / 40 GB).
	// Callers can override via K3sServerInput.InstanceType.
	defaultK3sServerInstanceType = "t3.medium"

	// k3sOIDCSigningKeyPath is the on-VM path where cloud-init drops the
	// per-cluster OIDC private key PEM. K3s reads it via the
	// service-account-key-file kube-apiserver arg baked into config.yaml.
	k3sOIDCSigningKeyPath = "/etc/rancher/k3s/oidc-signing-key.pem"

	// k3sFirstBootEnvPath is the env file k3s-first-boot.sh sources at boot
	// (see scripts/images/eks-server/k3s-first-boot.sh ENVFILE).
	k3sFirstBootEnvPath = "/etc/spinifex-eks/first-boot.env"

	// k3sNATSCAPath is the on-VM destination for the NATS CA cert PEM. The K3s
	// VM uses it to verify the daemon's NATS TLS when publishing bootstrap
	// messages. Path matches k3s-first-boot.sh SPINIFEX_NATS_CA.
	k3sNATSCAPath = "/etc/spinifex-eks/nats-ca.pem"

	// k3sConfigPath is the K3s server config file cloud-init writes; K3s
	// reads it at startup (overrides the AMI-baked config.yaml.skel).
	k3sConfigPath = "/etc/rancher/k3s/config.yaml"
)

// K3sServerInput is the launcher's input shape. AccountID is the customer
// account; the ENI + VM both live there in v1 (SystemAccount-owned VM is
// deferred behind cross-account-ENI work). Region is carried for future
// region-aware AMI lookups but not consumed today.
type K3sServerInput struct {
	AccountID         string
	ClusterName       string
	Region            string
	SubnetID          string
	ControlPlaneSGID  string
	NLBDNS            string
	OIDCIssuer        string
	OIDCPrivateKeyPEM string
	NATSURL           string
	NATSToken         string
	NATSCACert        string
	InstanceType      string
}

// K3sServerOutput carries the identifiers the caller needs to persist into
// ClusterMeta and to register the ENI IP with the cluster NLB target group.
type K3sServerOutput struct {
	InstanceID string
	ENIID      string
	ENIIP      string
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

	sysOut, err := instSvc.LaunchSystemInstance(&sysinstance.SystemInstanceInput{
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
		if err := instSvc.TerminateSystemInstance(instanceID); err != nil {
			slog.Warn("TerminateK3sServerVM: terminate failed", "instanceId", instanceID, "err", err)
			firstErr = fmt.Errorf("terminate instance %s: %w", instanceID, err)
		}
	}
	if eniID != "" {
		if _, err := vpcSvc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: aws.String(eniID),
		}, accountID); err != nil {
			slog.Warn("TerminateK3sServerVM: ENI delete failed", "eniId", eniID, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("delete ENI %s: %w", eniID, err)
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
	case in.NATSURL == "":
		return errors.New("eks: K3sServerInput empty NATSURL")
	case in.NATSToken == "":
		return errors.New("eks: K3sServerInput empty NATSToken")
	case strings.TrimSpace(in.NATSCACert) == "":
		return errors.New("eks: K3sServerInput empty NATSCACert")
	}
	return nil
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

	envBody := strings.Join([]string{
		"SPINIFEX_NATS_URL=" + in.NATSURL,
		"SPINIFEX_NATS_TOKEN=" + in.NATSToken,
		"SPINIFEX_NATS_CA=" + k3sNATSCAPath,
		"EKS_ACCOUNT_ID=" + in.AccountID,
		"EKS_CLUSTER_NAME=" + in.ClusterName,
		"EKS_NLB_ENDPOINT=" + nlbEndpoint,
		"EKS_OIDC_ISSUER=" + in.OIDCIssuer,
	}, "\n")

	k3sConfig := strings.Join([]string{
		"cluster-init: true",
		"tls-san:",
		"  - " + in.NLBDNS,
		"kube-apiserver-arg:",
		"  - service-account-key-file=" + k3sOIDCSigningKeyPath,
		"  - service-account-signing-key-file=" + k3sOIDCSigningKeyPath,
		"  - service-account-issuer=" + in.OIDCIssuer,
		"  - api-audiences=sts.amazonaws.com",
	}, "\n")

	files := []userDataFile{
		// first-boot.env carries the NATS token, so keep it root-only (0600).
		{Path: k3sFirstBootEnvPath, Perms: "0600", Body: envBody},
		{Path: k3sOIDCSigningKeyPath, Perms: "0600", Body: strings.TrimRight(in.OIDCPrivateKeyPEM, "\n")},
		{Path: k3sConfigPath, Perms: "0644", Body: k3sConfig},
		{Path: k3sNATSCAPath, Perms: "0644", Body: strings.TrimRight(in.NATSCACert, "\n")},
	}

	var buf strings.Builder
	buf.WriteString("#cloud-config\n")
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
