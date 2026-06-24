package handlers_eks

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

// nlbProvisioner is the narrow ELBv2 surface needed by cluster NLB helpers.
type nlbProvisioner interface {
	CreateLoadBalancer(input *elbv2.CreateLoadBalancerInput, accountID string) (*elbv2.CreateLoadBalancerOutput, error)
	CreateLoadBalancerSync(input *elbv2.CreateLoadBalancerInput, accountID string) (*elbv2.CreateLoadBalancerOutput, error)
	// CreateClusterNLBSync is CreateLoadBalancerSync plus cross-account ENIs threaded
	// onto the LB VM at launch (the customer-VPC Set A private-endpoint NIC).
	CreateClusterNLBSync(input *elbv2.CreateLoadBalancerInput, accountID string, crossAccountENIs []sysinstance.ExtraENIInput) (*elbv2.CreateLoadBalancerOutput, error)
	DescribeLoadBalancers(input *elbv2.DescribeLoadBalancersInput, accountID string) (*elbv2.DescribeLoadBalancersOutput, error)
	DeleteLoadBalancer(input *elbv2.DeleteLoadBalancerInput, accountID string) (*elbv2.DeleteLoadBalancerOutput, error)

	CreateTargetGroup(input *elbv2.CreateTargetGroupInput, accountID string) (*elbv2.CreateTargetGroupOutput, error)
	DescribeTargetGroups(input *elbv2.DescribeTargetGroupsInput, accountID string) (*elbv2.DescribeTargetGroupsOutput, error)
	DeleteTargetGroup(input *elbv2.DeleteTargetGroupInput, accountID string) (*elbv2.DeleteTargetGroupOutput, error)

	CreateListener(input *elbv2.CreateListenerInput, accountID string) (*elbv2.CreateListenerOutput, error)
	DescribeListeners(input *elbv2.DescribeListenersInput, accountID string) (*elbv2.DescribeListenersOutput, error)

	RegisterTargets(input *elbv2.RegisterTargetsInput, accountID string) (*elbv2.RegisterTargetsOutput, error)
	DeregisterTargets(input *elbv2.DeregisterTargetsInput, accountID string) (*elbv2.DeregisterTargetsOutput, error)

	SetLoadBalancerIngressCIDRs(lbArn string, cidrs []string, accountID string) error
}

// maxELBv2NameLen is the AWS limit on LB/TG names (alphanumeric + hyphens).
const maxELBv2NameLen = 32

// k3sAPIServerPort is the K3s apiserver port; NLB target group forwards here.
const k3sAPIServerPort int64 = 6443

// clusterNLBListenPort is the customer-facing port the cluster NLB exposes.
// kubectl + the AWS SDK both expect TLS on 443.
const clusterNLBListenPort int64 = 443

// konnectivityAgentPort is the konnectivity-server agent listen port. The NLB
// fronts it so worker konnectivity-agents reach the 3 CP konnectivity-servers
// through one VIP; server-count fan-out gives each apiserver a tunnel to every
// agent (HA apiserver->pod/kubelet/webhook egress, EKS-parity).
const konnectivityAgentPort int64 = 8132

// ClusterNLB is the NLB resource tuple produced by EnsureClusterNLB; ARNs are persisted to ClusterMeta.
type ClusterNLB struct {
	LoadBalancerArn string
	TargetGroupArn  string
	// KonnTargetGroupArn is the konnectivity-server target group (:8132 → 3 CP
	// servers). Empty until the konnectivity listener is provisioned.
	KonnTargetGroupArn string
	ListenerArn        string
	DNSName            string
	// FrontendIP is the public IP (internet-facing) or VPC IP (internal) of the NLB front-end.
	// Used as the kubeconfig endpoint host and apiserver cert SAN; empty if not yet provisioned.
	FrontendIP string
}

// ClusterNLBName returns the deterministic NLB name for a cluster.
func ClusterNLBName(clusterName string) string {
	return "eks-" + clusterName
}

// ClusterTargetGroupName returns the deterministic target-group name for a
// cluster's control-plane TG.
func ClusterTargetGroupName(clusterName string) string {
	return "eks-" + clusterName + "-cp"
}

// ClusterKonnTargetGroupName returns the deterministic target-group name for a
// cluster's konnectivity TG (:8132 → 3 CP servers).
func ClusterKonnTargetGroupName(clusterName string) string {
	return "eks-" + clusterName + "-konn"
}

// EnsureClusterNLB provisions (or returns) the cluster's NLB + target group +
// two TCP listeners (:443 and :6443), both forwarding to the CP target group.
// :443 serves kubectl/SDK; :6443 serves the in-cluster `kubernetes` Service
// Endpoints path (worker pods reach PrivateEndpointIP:6443). Idempotent on
// clusterName: existing resources are reused so DeleteCluster failure-retry and
// reconciler crash-recovery both converge without duplicate creates.
//
// The K3s server VM ENI IP is NOT registered here — Stage 4
// (k3s_server_vm.go) calls RegisterClusterTarget once the VM has its ENI IP.
//
// internetFacing selects the NLB scheme: true ⇒ internet-facing (external-pool
// front-end IP, LAN-reachable, for a public cluster endpoint); false ⇒ internal
// (VPC-only). The LB is created synchronously (CreateLoadBalancerSync), so its
// reachable FrontendIP is known on return and used as the endpoint host + cert
// SAN. A cluster whose NLB yields no front-end IP has no usable apiserver
// endpoint, so EnsureClusterNLB fails loud with ErrClusterNLBFrontendIPUnavailable
// rather than ship the unresolvable DNS-name fallback.
//
// publicAccessCidrs narrows who may reach an internet-facing front-end below the
// scheme default (0.0.0.0/0). It maps the cluster's publicAccessCidrs onto the
// NLB managed-SG ingress; ignored for an internal NLB (its ingress already
// tracks the VPC CIDR) and for the wide-open default, which the LB carries out
// of the box.
func EnsureClusterNLB(nlbp nlbProvisioner, accountID, clusterName string, subnetIDs []string, internetFacing bool, publicAccessCidrs []string, crossAccountENIs []sysinstance.ExtraENIInput) (*ClusterNLB, error) {
	if clusterName == "" {
		return nil, errors.New("eks: EnsureClusterNLB empty cluster name")
	}
	if len(subnetIDs) == 0 {
		return nil, errors.New("eks: EnsureClusterNLB empty subnet list")
	}
	lbName := ClusterNLBName(clusterName)
	tgName := ClusterTargetGroupName(clusterName)
	konnTGName := ClusterKonnTargetGroupName(clusterName)
	for _, n := range []string{lbName, tgName, konnTGName} {
		if len(n) > maxELBv2NameLen {
			return nil, fmt.Errorf("eks: ELBv2 name %q exceeds %d chars (cluster name too long)", n, maxELBv2NameLen)
		}
	}

	out := &ClusterNLB{}

	if err := ensureClusterLB(nlbp, accountID, clusterName, lbName, subnetIDs, internetFacing, crossAccountENIs, out); err != nil {
		return nil, err
	}
	var err error
	if out.TargetGroupArn, err = ensureClusterTG(nlbp, accountID, clusterName, tgName, k3sAPIServerPort); err != nil {
		return nil, err
	}
	if out.ListenerArn, err = ensureClusterListener(nlbp, accountID, lbName, out.LoadBalancerArn, out.TargetGroupArn, clusterNLBListenPort); err != nil {
		return nil, err
	}
	// Second listener :6443 → same CP target group. The apiserver advertises itself
	// on :6443 in the in-cluster `kubernetes` Endpoints; worker pods reach it only if
	// the NLB serves :6443. Arn not persisted — teardown cascades via DeleteLoadBalancer.
	if _, err = ensureClusterListener(nlbp, accountID, lbName, out.LoadBalancerArn, out.TargetGroupArn, k3sAPIServerPort); err != nil {
		return nil, err
	}
	// Konnectivity TG + listener :8132 → the 3 CP konnectivity-servers. Worker
	// konnectivity-agents dial this VIP; server-count fan-out reaches all 3 servers.
	if out.KonnTargetGroupArn, err = ensureClusterTG(nlbp, accountID, clusterName, konnTGName, konnectivityAgentPort); err != nil {
		return nil, err
	}
	if _, err = ensureClusterListener(nlbp, accountID, lbName, out.LoadBalancerArn, out.KonnTargetGroupArn, konnectivityAgentPort); err != nil {
		return nil, err
	}
	if internetFacing && narrowsPublicAccess(publicAccessCidrs) {
		if err := nlbp.SetLoadBalancerIngressCIDRs(out.LoadBalancerArn, publicAccessCidrs, accountID); err != nil {
			return nil, fmt.Errorf("eks: set NLB ingress CIDRs for %s: %w", lbName, err)
		}
	}
	// FrontendIP is set synchronously by ensureClusterLB. An empty value means the
	// backing LB never got a reachable address (e.g. no external IP pool), which
	// would leave the apiserver cert SANed only to the unresolvable DNS name —
	// fail the launch loud instead.
	if out.FrontendIP == "" {
		return nil, fmt.Errorf("eks: NLB %s: %w", lbName, ErrClusterNLBFrontendIPUnavailable)
	}
	return out, nil
}

// ErrClusterNLBFrontendIPUnavailable is returned by EnsureClusterNLB when the
// cluster's NLB came up without a reachable front-end IP, so no usable apiserver
// endpoint (with a matching cert SAN) can be published.
var ErrClusterNLBFrontendIPUnavailable = errors.New("eks: cluster NLB has no reachable front-end IP")

// narrowsPublicAccess reports whether publicAccessCidrs restrict the front-end
// below the wide-open default an internet-facing NLB already carries. Empty (no
// override) or the lone 0.0.0.0/0 default are no-ops and skip the extra ELBv2
// round-trip.
func narrowsPublicAccess(cidrs []string) bool {
	if len(cidrs) == 0 {
		return false
	}
	if len(cidrs) == 1 && cidrs[0] == defaultPublicAccessCidr {
		return false
	}
	return true
}

// frontendIPFromLB returns the public IpAddress (internet-facing) or PrivateIPv4Address (internal)
// from the described LB; first non-empty match across AZs.
func frontendIPFromLB(lb *elbv2.LoadBalancer, internetFacing bool) string {
	for _, az := range lb.AvailabilityZones {
		for _, addr := range az.LoadBalancerAddresses {
			if addr == nil {
				continue
			}
			if internetFacing {
				if ip := aws.StringValue(addr.IpAddress); ip != "" {
					return ip
				}
				continue
			}
			if ip := aws.StringValue(addr.PrivateIPv4Address); ip != "" {
				return ip
			}
		}
	}
	return ""
}

// RegisterClusterTarget attaches one ENI IP to the cluster TG on port.
func RegisterClusterTarget(nlbp nlbProvisioner, accountID, tgArn, eniIP string, port int64) error {
	if eniIP == "" {
		return errors.New("eks: RegisterClusterTarget empty ENI IP")
	}
	return RegisterClusterTargets(nlbp, accountID, tgArn, []string{eniIP}, port)
}

// RegisterClusterTargets attaches all CP ENI IPs to the cluster TG on port. Empty
// IPs are skipped; RegisterTargets deduplicates on (id, port) so re-invocation is
// idempotent.
func RegisterClusterTargets(nlbp nlbProvisioner, accountID, tgArn string, eniIPs []string, port int64) error {
	if tgArn == "" {
		return errors.New("eks: RegisterClusterTargets empty TG arn")
	}
	targets := make([]*elbv2.TargetDescription, 0, len(eniIPs))
	for _, ip := range eniIPs {
		if ip == "" {
			continue
		}
		targets = append(targets, &elbv2.TargetDescription{
			Id:   aws.String(ip),
			Port: aws.Int64(port),
		})
	}
	if len(targets) == 0 {
		return errors.New("eks: RegisterClusterTargets no non-empty ENI IPs")
	}
	if _, err := nlbp.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(tgArn),
		Targets:        targets,
	}, accountID); err != nil {
		return fmt.Errorf("register %d targets on TG %s: %w", len(targets), tgArn, err)
	}
	return nil
}

// DeregisterClusterTarget removes an ENI IP from the cluster TG (on port) before VM termination.
func DeregisterClusterTarget(nlbp nlbProvisioner, accountID, tgArn, eniIP string, port int64) error {
	if tgArn == "" {
		return errors.New("eks: DeregisterClusterTarget empty TG arn")
	}
	if eniIP == "" {
		return errors.New("eks: DeregisterClusterTarget empty ENI IP")
	}
	if _, err := nlbp.DeregisterTargets(&elbv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String(tgArn),
		Targets: []*elbv2.TargetDescription{{
			Id:   aws.String(eniIP),
			Port: aws.Int64(port),
		}},
	}, accountID); err != nil {
		return fmt.Errorf("deregister target %s on TG %s: %w", eniIP, tgArn, err)
	}
	return nil
}

// DeleteClusterNLB tears down the cluster NLB and TG. DeleteLoadBalancer cascades
// listener cleanup. Missing resources are no-ops; LB error takes precedence if both fail.
func DeleteClusterNLB(nlbp nlbProvisioner, accountID, clusterName string) error {
	if clusterName == "" {
		return errors.New("eks: DeleteClusterNLB empty cluster name")
	}
	lbErr := deleteClusterLB(nlbp, accountID, ClusterNLBName(clusterName))
	tgErr := deleteClusterTG(nlbp, accountID, ClusterTargetGroupName(clusterName))
	// The konnectivity TG is a no-op for clusters created before this topology.
	konnErr := deleteClusterTG(nlbp, accountID, ClusterKonnTargetGroupName(clusterName))
	switch {
	case lbErr != nil:
		return lbErr
	case tgErr != nil:
		return tgErr
	default:
		return konnErr
	}
}

func deleteClusterLB(nlbp nlbProvisioner, accountID, lbName string) error {
	lb, err := lookupLBByName(nlbp, accountID, lbName)
	if err != nil {
		slog.Warn("DeleteClusterNLB: LB lookup failed", "name", lbName, "err", err)
		return err
	}
	if lb == nil || lb.LoadBalancerArn == nil {
		return nil
	}
	if _, err := nlbp.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{LoadBalancerArn: lb.LoadBalancerArn}, accountID); err != nil {
		slog.Warn("DeleteClusterNLB: delete NLB failed", "name", lbName, "err", err)
		return fmt.Errorf("delete NLB %s: %w", lbName, err)
	}
	return nil
}

func deleteClusterTG(nlbp nlbProvisioner, accountID, tgName string) error {
	tg, err := lookupTGByName(nlbp, accountID, tgName)
	if err != nil {
		slog.Warn("DeleteClusterNLB: TG lookup failed", "name", tgName, "err", err)
		return err
	}
	if tg == nil || tg.TargetGroupArn == nil {
		return nil
	}
	if _, err := nlbp.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{TargetGroupArn: tg.TargetGroupArn}, accountID); err != nil {
		slog.Warn("DeleteClusterNLB: delete TG failed", "name", tgName, "err", err)
		return fmt.Errorf("delete TG %s: %w", tgName, err)
	}
	return nil
}

func ensureClusterLB(nlbp nlbProvisioner, accountID, clusterName, lbName string, subnetIDs []string, internetFacing bool, crossAccountENIs []sysinstance.ExtraENIInput, out *ClusterNLB) error {
	if lb, err := lookupLBByName(nlbp, accountID, lbName); err != nil {
		return err
	} else if lb != nil {
		// Idempotent re-entry: the LB already exists (and, since creation is
		// synchronous, has already been launched), so its address is on the record.
		out.LoadBalancerArn = aws.StringValue(lb.LoadBalancerArn)
		out.DNSName = aws.StringValue(lb.DNSName)
		out.FrontendIP = frontendIPFromLB(lb, internetFacing)
		if out.LoadBalancerArn == "" || out.DNSName == "" {
			return fmt.Errorf("eks: existing NLB %s missing arn or DNS name", lbName)
		}
		return nil
	}

	scheme := elbv2.LoadBalancerSchemeEnumInternal
	if internetFacing {
		scheme = elbv2.LoadBalancerSchemeEnumInternetFacing
	}
	// Synchronous create: the cluster launch runs off the gateway responder, so it
	// awaits the data-plane launch and gets back an LB carrying its front-end IP —
	// which must be baked into the apiserver cert SAN before the control-plane VM
	// boots (the async CreateLoadBalancer would return before the IP is allocated).
	// Cross-account ENIs (the Set A private-endpoint NIC) are threaded onto the LB
	// VM at launch, so the create must carry them when present.
	in := &elbv2.CreateLoadBalancerInput{
		Name:    aws.String(lbName),
		Type:    aws.String(elbv2.LoadBalancerTypeEnumNetwork),
		Scheme:  aws.String(scheme),
		Subnets: aws.StringSlice(subnetIDs),
		Tags: []*elbv2.Tag{
			{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
			{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(clusterName)},
		},
	}
	var created *elbv2.CreateLoadBalancerOutput
	var err error
	if len(crossAccountENIs) > 0 {
		created, err = nlbp.CreateClusterNLBSync(in, accountID, crossAccountENIs)
	} else {
		created, err = nlbp.CreateLoadBalancerSync(in, accountID)
	}
	if err != nil {
		return fmt.Errorf("create NLB %s: %w", lbName, err)
	}
	if created == nil || len(created.LoadBalancers) == 0 || created.LoadBalancers[0] == nil {
		return fmt.Errorf("eks: CreateLoadBalancer returned no LB for %s", lbName)
	}
	lb := created.LoadBalancers[0]
	out.LoadBalancerArn = aws.StringValue(lb.LoadBalancerArn)
	out.DNSName = aws.StringValue(lb.DNSName)
	out.FrontendIP = frontendIPFromLB(lb, internetFacing)
	if out.LoadBalancerArn == "" || out.DNSName == "" {
		return fmt.Errorf("eks: CreateLoadBalancer returned empty arn or DNS name for %s", lbName)
	}
	return nil
}

// ensureClusterTG creates (or reuses) a TCP target group of the given name
// forwarding to port, returning its ARN. Idempotent on name.
func ensureClusterTG(nlbp nlbProvisioner, accountID, clusterName, tgName string, port int64) (string, error) {
	if tg, err := lookupTGByName(nlbp, accountID, tgName); err != nil {
		return "", err
	} else if tg != nil {
		arn := aws.StringValue(tg.TargetGroupArn)
		if arn == "" {
			return "", fmt.Errorf("eks: existing TG %s missing arn", tgName)
		}
		return arn, nil
	}

	created, err := nlbp.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:       aws.String(tgName),
		Protocol:   aws.String(elbv2.ProtocolEnumTcp),
		Port:       aws.Int64(port),
		TargetType: aws.String(elbv2.TargetTypeEnumIp),
		Tags: []*elbv2.Tag{
			{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
			{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(clusterName)},
		},
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("create TG %s: %w", tgName, err)
	}
	if created == nil || len(created.TargetGroups) == 0 || created.TargetGroups[0] == nil {
		return "", fmt.Errorf("eks: CreateTargetGroup returned no TG for %s", tgName)
	}
	arn := aws.StringValue(created.TargetGroups[0].TargetGroupArn)
	if arn == "" {
		return "", fmt.Errorf("eks: CreateTargetGroup returned empty arn for %s", tgName)
	}
	return arn, nil
}

// ensureClusterListener creates (or reuses) one TCP listener on the LB at the
// given port forwarding to tgArn, returning its ARN. Idempotent: an existing
// listener on the port is reused.
func ensureClusterListener(nlbp nlbProvisioner, accountID, lbName, lbArn, tgArn string, port int64) (string, error) {
	if l, err := lookupListenerByPort(nlbp, accountID, lbArn, port); err != nil {
		return "", err
	} else if l != nil {
		arn := aws.StringValue(l.ListenerArn)
		if arn == "" {
			return "", fmt.Errorf("eks: existing listener for %s:%d missing arn", lbName, port)
		}
		return arn, nil
	}

	created, err := nlbp.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Protocol:        aws.String(elbv2.ProtocolEnumTcp),
		Port:            aws.Int64(port),
		DefaultActions: []*elbv2.Action{{
			Type:           aws.String(elbv2.ActionTypeEnumForward),
			TargetGroupArn: aws.String(tgArn),
		}},
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("create listener %s:%d: %w", lbName, port, err)
	}
	if created == nil || len(created.Listeners) == 0 || created.Listeners[0] == nil {
		return "", fmt.Errorf("eks: CreateListener returned no listener for %s:%d", lbName, port)
	}
	arn := aws.StringValue(created.Listeners[0].ListenerArn)
	if arn == "" {
		return "", fmt.Errorf("eks: CreateListener returned empty arn for %s:%d", lbName, port)
	}
	return arn, nil
}

func lookupLBByName(nlbp nlbProvisioner, accountID, name string) (*elbv2.LoadBalancer, error) {
	out, err := nlbp.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
		Names: aws.StringSlice([]string{name}),
	}, accountID)
	if err != nil {
		return nil, fmt.Errorf("describe NLB %s: %w", name, err)
	}
	for _, lb := range out.LoadBalancers {
		if lb != nil {
			return lb, nil
		}
	}
	return nil, nil
}

func lookupTGByName(nlbp nlbProvisioner, accountID, name string) (*elbv2.TargetGroup, error) {
	out, err := nlbp.DescribeTargetGroups(&elbv2.DescribeTargetGroupsInput{
		Names: aws.StringSlice([]string{name}),
	}, accountID)
	if err != nil {
		return nil, fmt.Errorf("describe TG %s: %w", name, err)
	}
	for _, tg := range out.TargetGroups {
		if tg != nil {
			return tg, nil
		}
	}
	return nil, nil
}

func lookupListenerByPort(nlbp nlbProvisioner, accountID, lbArn string, port int64) (*elbv2.Listener, error) {
	if lbArn == "" {
		return nil, errors.New("eks: lookupListenerByPort empty LB arn")
	}
	out, err := nlbp.DescribeListeners(&elbv2.DescribeListenersInput{
		LoadBalancerArn: aws.String(lbArn),
	}, accountID)
	if err != nil {
		return nil, fmt.Errorf("describe listeners on %s: %w", lbArn, err)
	}
	for _, l := range out.Listeners {
		if l == nil || l.Port == nil {
			continue
		}
		if *l.Port == port {
			return l, nil
		}
	}
	return nil, nil
}
