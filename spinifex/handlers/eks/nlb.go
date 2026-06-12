package handlers_eks

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

// nlbProvisioner is the narrow ELBv2 surface needed by cluster NLB helpers.
type nlbProvisioner interface {
	CreateLoadBalancer(input *elbv2.CreateLoadBalancerInput, accountID string) (*elbv2.CreateLoadBalancerOutput, error)
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

// ClusterNLB is the NLB resource tuple produced by EnsureClusterNLB; ARNs are persisted to ClusterMeta.
type ClusterNLB struct {
	LoadBalancerArn string
	TargetGroupArn  string
	ListenerArn     string
	DNSName         string
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

// EnsureClusterNLB provisions (or reuses) the cluster NLB, target group, and
// :443→:6443 TCP listener. Idempotent; ENI registration is done separately by
// RegisterClusterTarget. publicAccessCidrs narrows ingress for internet-facing LBs.
func EnsureClusterNLB(nlbp nlbProvisioner, accountID, clusterName string, subnetIDs []string, internetFacing bool, publicAccessCidrs []string) (*ClusterNLB, error) {
	if clusterName == "" {
		return nil, errors.New("eks: EnsureClusterNLB empty cluster name")
	}
	if len(subnetIDs) == 0 {
		return nil, errors.New("eks: EnsureClusterNLB empty subnet list")
	}
	lbName := ClusterNLBName(clusterName)
	tgName := ClusterTargetGroupName(clusterName)
	if len(lbName) > maxELBv2NameLen {
		return nil, fmt.Errorf("eks: NLB name %q exceeds %d chars (cluster name too long)", lbName, maxELBv2NameLen)
	}
	if len(tgName) > maxELBv2NameLen {
		return nil, fmt.Errorf("eks: TG name %q exceeds %d chars (cluster name too long)", tgName, maxELBv2NameLen)
	}

	out := &ClusterNLB{}

	if err := ensureClusterLB(nlbp, accountID, clusterName, lbName, subnetIDs, internetFacing, out); err != nil {
		return nil, err
	}
	if err := ensureClusterTG(nlbp, accountID, clusterName, tgName, out); err != nil {
		return nil, err
	}
	if err := ensureClusterListener(nlbp, accountID, lbName, out); err != nil {
		return nil, err
	}
	if internetFacing && narrowsPublicAccess(publicAccessCidrs) {
		if err := nlbp.SetLoadBalancerIngressCIDRs(out.LoadBalancerArn, publicAccessCidrs, accountID); err != nil {
			return nil, fmt.Errorf("eks: set NLB ingress CIDRs for %s: %w", lbName, err)
		}
	}
	// Best-effort front-end IP read-back; caller falls back to DNS name if empty.
	if lb, err := lookupLBByName(nlbp, accountID, lbName); err == nil && lb != nil {
		out.FrontendIP = frontendIPFromLB(lb, internetFacing)
	} else if err != nil {
		slog.Warn("EnsureClusterNLB: front-end IP read-back failed", "name", lbName, "err", err)
	}
	return out, nil
}

// narrowsPublicAccess reports whether publicAccessCidrs restrict below the 0.0.0.0/0 default.
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

// RegisterClusterTarget attaches one ENI IP to the cluster TG.
func RegisterClusterTarget(nlbp nlbProvisioner, accountID, tgArn, eniIP string) error {
	if eniIP == "" {
		return errors.New("eks: RegisterClusterTarget empty ENI IP")
	}
	return RegisterClusterTargets(nlbp, accountID, tgArn, []string{eniIP})
}

// RegisterClusterTargets attaches all CP ENI IPs to the cluster TG. Empty IPs are
// skipped; RegisterTargets deduplicates on (id, port) so re-invocation is idempotent.
func RegisterClusterTargets(nlbp nlbProvisioner, accountID, tgArn string, eniIPs []string) error {
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
			Port: aws.Int64(k3sAPIServerPort),
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

// DeregisterClusterTarget removes an ENI IP from the cluster TG before VM termination.
func DeregisterClusterTarget(nlbp nlbProvisioner, accountID, tgArn, eniIP string) error {
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
			Port: aws.Int64(k3sAPIServerPort),
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
	if lbErr != nil {
		return lbErr
	}
	return tgErr
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

func ensureClusterLB(nlbp nlbProvisioner, accountID, clusterName, lbName string, subnetIDs []string, internetFacing bool, out *ClusterNLB) error {
	if lb, err := lookupLBByName(nlbp, accountID, lbName); err != nil {
		return err
	} else if lb != nil {
		out.LoadBalancerArn = aws.StringValue(lb.LoadBalancerArn)
		out.DNSName = aws.StringValue(lb.DNSName)
		if out.LoadBalancerArn == "" || out.DNSName == "" {
			return fmt.Errorf("eks: existing NLB %s missing arn or DNS name", lbName)
		}
		return nil
	}

	scheme := elbv2.LoadBalancerSchemeEnumInternal
	if internetFacing {
		scheme = elbv2.LoadBalancerSchemeEnumInternetFacing
	}
	created, err := nlbp.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:    aws.String(lbName),
		Type:    aws.String(elbv2.LoadBalancerTypeEnumNetwork),
		Scheme:  aws.String(scheme),
		Subnets: aws.StringSlice(subnetIDs),
		Tags: []*elbv2.Tag{
			{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
			{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(clusterName)},
		},
	}, accountID)
	if err != nil {
		return fmt.Errorf("create NLB %s: %w", lbName, err)
	}
	if created == nil || len(created.LoadBalancers) == 0 || created.LoadBalancers[0] == nil {
		return fmt.Errorf("eks: CreateLoadBalancer returned no LB for %s", lbName)
	}
	lb := created.LoadBalancers[0]
	out.LoadBalancerArn = aws.StringValue(lb.LoadBalancerArn)
	out.DNSName = aws.StringValue(lb.DNSName)
	if out.LoadBalancerArn == "" || out.DNSName == "" {
		return fmt.Errorf("eks: CreateLoadBalancer returned empty arn or DNS name for %s", lbName)
	}
	return nil
}

func ensureClusterTG(nlbp nlbProvisioner, accountID, clusterName, tgName string, out *ClusterNLB) error {
	if tg, err := lookupTGByName(nlbp, accountID, tgName); err != nil {
		return err
	} else if tg != nil {
		out.TargetGroupArn = aws.StringValue(tg.TargetGroupArn)
		if out.TargetGroupArn == "" {
			return fmt.Errorf("eks: existing TG %s missing arn", tgName)
		}
		return nil
	}

	created, err := nlbp.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:       aws.String(tgName),
		Protocol:   aws.String(elbv2.ProtocolEnumTcp),
		Port:       aws.Int64(k3sAPIServerPort),
		TargetType: aws.String(elbv2.TargetTypeEnumIp),
		Tags: []*elbv2.Tag{
			{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
			{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(clusterName)},
		},
	}, accountID)
	if err != nil {
		return fmt.Errorf("create TG %s: %w", tgName, err)
	}
	if created == nil || len(created.TargetGroups) == 0 || created.TargetGroups[0] == nil {
		return fmt.Errorf("eks: CreateTargetGroup returned no TG for %s", tgName)
	}
	out.TargetGroupArn = aws.StringValue(created.TargetGroups[0].TargetGroupArn)
	if out.TargetGroupArn == "" {
		return fmt.Errorf("eks: CreateTargetGroup returned empty arn for %s", tgName)
	}
	return nil
}

func ensureClusterListener(nlbp nlbProvisioner, accountID, lbName string, out *ClusterNLB) error {
	if l, err := lookupListenerByPort(nlbp, accountID, out.LoadBalancerArn, clusterNLBListenPort); err != nil {
		return err
	} else if l != nil {
		out.ListenerArn = aws.StringValue(l.ListenerArn)
		if out.ListenerArn == "" {
			return fmt.Errorf("eks: existing listener for %s:%d missing arn", lbName, clusterNLBListenPort)
		}
		return nil
	}

	created, err := nlbp.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(out.LoadBalancerArn),
		Protocol:        aws.String(elbv2.ProtocolEnumTcp),
		Port:            aws.Int64(clusterNLBListenPort),
		DefaultActions: []*elbv2.Action{{
			Type:           aws.String(elbv2.ActionTypeEnumForward),
			TargetGroupArn: aws.String(out.TargetGroupArn),
		}},
	}, accountID)
	if err != nil {
		return fmt.Errorf("create listener %s:%d: %w", lbName, clusterNLBListenPort, err)
	}
	if created == nil || len(created.Listeners) == 0 || created.Listeners[0] == nil {
		return fmt.Errorf("eks: CreateListener returned no listener for %s:%d", lbName, clusterNLBListenPort)
	}
	out.ListenerArn = aws.StringValue(created.Listeners[0].ListenerArn)
	if out.ListenerArn == "" {
		return fmt.Errorf("eks: CreateListener returned empty arn for %s:%d", lbName, clusterNLBListenPort)
	}
	return nil
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
