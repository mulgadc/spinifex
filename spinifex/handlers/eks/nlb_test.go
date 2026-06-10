package handlers_eks

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeNLBProvisioner struct {
	createLBCalls       []*elbv2.CreateLoadBalancerInput
	describeLBCalls     []*elbv2.DescribeLoadBalancersInput
	deleteLBCalls       []*elbv2.DeleteLoadBalancerInput
	createTGCalls       []*elbv2.CreateTargetGroupInput
	describeTGCalls     []*elbv2.DescribeTargetGroupsInput
	deleteTGCalls       []*elbv2.DeleteTargetGroupInput
	createListenerCalls []*elbv2.CreateListenerInput
	describeListeners   []*elbv2.DescribeListenersInput
	registerCalls       []*elbv2.RegisterTargetsInput
	deregisterCalls     []*elbv2.DeregisterTargetsInput
	setIngressCalls     []setIngressCIDRsCall

	lbByName       map[string]*elbv2.LoadBalancer
	tgByName       map[string]*elbv2.TargetGroup
	listenerByPort map[string]map[int64]*elbv2.Listener // lbArn → port → listener

	createLBOut       *elbv2.CreateLoadBalancerOutput
	createTGOut       *elbv2.CreateTargetGroupOutput
	createListenerOut *elbv2.CreateListenerOutput
	createLBErr       error
	createTGErr       error
	createListenerErr error
	deleteLBErr       error
	deleteTGErr       error
	registerErr       error
	deregisterErr     error
	setIngressErr     error
}

type setIngressCIDRsCall struct {
	lbArn string
	cidrs []string
}

var _ nlbProvisioner = (*fakeNLBProvisioner)(nil)

func newFakeNLBProvisioner() *fakeNLBProvisioner {
	return &fakeNLBProvisioner{
		lbByName:       map[string]*elbv2.LoadBalancer{},
		tgByName:       map[string]*elbv2.TargetGroup{},
		listenerByPort: map[string]map[int64]*elbv2.Listener{},
	}
}

func (f *fakeNLBProvisioner) CreateLoadBalancer(input *elbv2.CreateLoadBalancerInput, _ string) (*elbv2.CreateLoadBalancerOutput, error) {
	f.createLBCalls = append(f.createLBCalls, input)
	if f.createLBErr != nil {
		return nil, f.createLBErr
	}
	if f.createLBOut == nil {
		name := aws.StringValue(input.Name)
		arn := "arn:aws:elasticloadbalancing:us-east-1:111122223333:loadbalancer/net/" + name + "/lb-001"
		dns := name + "-lb-001.us-east-1.elb.spinifex.local"
		f.createLBOut = &elbv2.CreateLoadBalancerOutput{
			LoadBalancers: []*elbv2.LoadBalancer{{
				LoadBalancerArn:  aws.String(arn),
				LoadBalancerName: aws.String(name),
				DNSName:          aws.String(dns),
				Type:             aws.String(elbv2.LoadBalancerTypeEnumNetwork),
				Scheme:           aws.String(elbv2.LoadBalancerSchemeEnumInternal),
			}},
		}
	}
	out := f.createLBOut
	if len(out.LoadBalancers) > 0 {
		name := aws.StringValue(out.LoadBalancers[0].LoadBalancerName)
		f.lbByName[name] = out.LoadBalancers[0]
	}
	return out, nil
}

func (f *fakeNLBProvisioner) DescribeLoadBalancers(input *elbv2.DescribeLoadBalancersInput, _ string) (*elbv2.DescribeLoadBalancersOutput, error) {
	f.describeLBCalls = append(f.describeLBCalls, input)
	out := &elbv2.DescribeLoadBalancersOutput{}
	for _, n := range input.Names {
		if n == nil {
			continue
		}
		if lb, ok := f.lbByName[*n]; ok {
			out.LoadBalancers = append(out.LoadBalancers, lb)
		}
	}
	return out, nil
}

func (f *fakeNLBProvisioner) DeleteLoadBalancer(input *elbv2.DeleteLoadBalancerInput, _ string) (*elbv2.DeleteLoadBalancerOutput, error) {
	f.deleteLBCalls = append(f.deleteLBCalls, input)
	if f.deleteLBErr != nil {
		return nil, f.deleteLBErr
	}
	if input.LoadBalancerArn != nil {
		for name, lb := range f.lbByName {
			if aws.StringValue(lb.LoadBalancerArn) == *input.LoadBalancerArn {
				delete(f.lbByName, name)
			}
		}
	}
	return &elbv2.DeleteLoadBalancerOutput{}, nil
}

func (f *fakeNLBProvisioner) CreateTargetGroup(input *elbv2.CreateTargetGroupInput, _ string) (*elbv2.CreateTargetGroupOutput, error) {
	f.createTGCalls = append(f.createTGCalls, input)
	if f.createTGErr != nil {
		return nil, f.createTGErr
	}
	if f.createTGOut == nil {
		name := aws.StringValue(input.Name)
		arn := "arn:aws:elasticloadbalancing:us-east-1:111122223333:targetgroup/" + name + "/tg-001"
		f.createTGOut = &elbv2.CreateTargetGroupOutput{
			TargetGroups: []*elbv2.TargetGroup{{
				TargetGroupArn:  aws.String(arn),
				TargetGroupName: aws.String(name),
				Protocol:        input.Protocol,
				Port:            input.Port,
				TargetType:      input.TargetType,
			}},
		}
	}
	out := f.createTGOut
	if len(out.TargetGroups) > 0 {
		name := aws.StringValue(out.TargetGroups[0].TargetGroupName)
		f.tgByName[name] = out.TargetGroups[0]
	}
	return out, nil
}

func (f *fakeNLBProvisioner) DescribeTargetGroups(input *elbv2.DescribeTargetGroupsInput, _ string) (*elbv2.DescribeTargetGroupsOutput, error) {
	f.describeTGCalls = append(f.describeTGCalls, input)
	out := &elbv2.DescribeTargetGroupsOutput{}
	for _, n := range input.Names {
		if n == nil {
			continue
		}
		if tg, ok := f.tgByName[*n]; ok {
			out.TargetGroups = append(out.TargetGroups, tg)
		}
	}
	return out, nil
}

func (f *fakeNLBProvisioner) DeleteTargetGroup(input *elbv2.DeleteTargetGroupInput, _ string) (*elbv2.DeleteTargetGroupOutput, error) {
	f.deleteTGCalls = append(f.deleteTGCalls, input)
	if f.deleteTGErr != nil {
		return nil, f.deleteTGErr
	}
	if input.TargetGroupArn != nil {
		for name, tg := range f.tgByName {
			if aws.StringValue(tg.TargetGroupArn) == *input.TargetGroupArn {
				delete(f.tgByName, name)
			}
		}
	}
	return &elbv2.DeleteTargetGroupOutput{}, nil
}

func (f *fakeNLBProvisioner) CreateListener(input *elbv2.CreateListenerInput, _ string) (*elbv2.CreateListenerOutput, error) {
	f.createListenerCalls = append(f.createListenerCalls, input)
	if f.createListenerErr != nil {
		return nil, f.createListenerErr
	}
	if f.createListenerOut == nil {
		lbArn := aws.StringValue(input.LoadBalancerArn)
		port := aws.Int64Value(input.Port)
		listenerArn := lbArn + "/listener/lst-001"
		f.createListenerOut = &elbv2.CreateListenerOutput{
			Listeners: []*elbv2.Listener{{
				ListenerArn:     aws.String(listenerArn),
				LoadBalancerArn: input.LoadBalancerArn,
				Port:            input.Port,
				Protocol:        input.Protocol,
				DefaultActions:  input.DefaultActions,
			}},
		}
		if f.listenerByPort[lbArn] == nil {
			f.listenerByPort[lbArn] = map[int64]*elbv2.Listener{}
		}
		f.listenerByPort[lbArn][port] = f.createListenerOut.Listeners[0]
	}
	return f.createListenerOut, nil
}

func (f *fakeNLBProvisioner) DescribeListeners(input *elbv2.DescribeListenersInput, _ string) (*elbv2.DescribeListenersOutput, error) {
	f.describeListeners = append(f.describeListeners, input)
	out := &elbv2.DescribeListenersOutput{}
	if input.LoadBalancerArn != nil {
		for _, l := range f.listenerByPort[*input.LoadBalancerArn] {
			out.Listeners = append(out.Listeners, l)
		}
	}
	return out, nil
}

func (f *fakeNLBProvisioner) RegisterTargets(input *elbv2.RegisterTargetsInput, _ string) (*elbv2.RegisterTargetsOutput, error) {
	f.registerCalls = append(f.registerCalls, input)
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	return &elbv2.RegisterTargetsOutput{}, nil
}

func (f *fakeNLBProvisioner) DeregisterTargets(input *elbv2.DeregisterTargetsInput, _ string) (*elbv2.DeregisterTargetsOutput, error) {
	f.deregisterCalls = append(f.deregisterCalls, input)
	if f.deregisterErr != nil {
		return nil, f.deregisterErr
	}
	return &elbv2.DeregisterTargetsOutput{}, nil
}

func (f *fakeNLBProvisioner) SetLoadBalancerIngressCIDRs(lbArn string, cidrs []string, _ string) error {
	f.setIngressCalls = append(f.setIngressCalls, setIngressCIDRsCall{lbArn: lbArn, cidrs: cidrs})
	return f.setIngressErr
}

func TestEnsureClusterNLB_EmptyInputsRejected(t *testing.T) {
	nlbp := newFakeNLBProvisioner()

	_, err := EnsureClusterNLB(nlbp, "111122223333", "", []string{"subnet-aaa"}, false, nil)
	require.Error(t, err)

	_, err = EnsureClusterNLB(nlbp, "111122223333", "alpha", nil, false, nil)
	require.Error(t, err)

	assert.Empty(t, nlbp.createLBCalls)
}

func TestEnsureClusterNLB_NameTooLongRejected(t *testing.T) {
	nlbp := newFakeNLBProvisioner()

	longName := strings.Repeat("x", maxELBv2NameLen) // "eks-" + 32x = 36 chars
	_, err := EnsureClusterNLB(nlbp, "111122223333", longName, []string{"subnet-aaa"}, false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
	assert.Empty(t, nlbp.createLBCalls)
}

func TestEnsureClusterNLB_FreshCreatesAllThree(t *testing.T) {
	nlbp := newFakeNLBProvisioner()

	out, err := EnsureClusterNLB(nlbp, "111122223333", "alpha", []string{"subnet-aaa", "subnet-bbb"}, false, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.NotEmpty(t, out.LoadBalancerArn)
	assert.NotEmpty(t, out.TargetGroupArn)
	assert.NotEmpty(t, out.ListenerArn)
	assert.Contains(t, out.DNSName, "eks-alpha")

	require.Len(t, nlbp.createLBCalls, 1)
	lbIn := nlbp.createLBCalls[0]
	assert.Equal(t, "eks-alpha", aws.StringValue(lbIn.Name))
	assert.Equal(t, elbv2.LoadBalancerTypeEnumNetwork, aws.StringValue(lbIn.Type))
	assert.Equal(t, elbv2.LoadBalancerSchemeEnumInternal, aws.StringValue(lbIn.Scheme))
	assert.Equal(t, []string{"subnet-aaa", "subnet-bbb"}, aws.StringValueSlice(lbIn.Subnets))
	assertELBv2TaggedAsEKS(t, lbIn.Tags, "alpha")

	require.Len(t, nlbp.createTGCalls, 1)
	tgIn := nlbp.createTGCalls[0]
	assert.Equal(t, "eks-alpha-cp", aws.StringValue(tgIn.Name))
	assert.Equal(t, elbv2.ProtocolEnumTcp, aws.StringValue(tgIn.Protocol))
	assert.Equal(t, k3sAPIServerPort, aws.Int64Value(tgIn.Port))
	assert.Equal(t, elbv2.TargetTypeEnumIp, aws.StringValue(tgIn.TargetType))
	assertELBv2TaggedAsEKS(t, tgIn.Tags, "alpha")

	require.Len(t, nlbp.createListenerCalls, 1)
	lstIn := nlbp.createListenerCalls[0]
	assert.Equal(t, out.LoadBalancerArn, aws.StringValue(lstIn.LoadBalancerArn))
	assert.Equal(t, elbv2.ProtocolEnumTcp, aws.StringValue(lstIn.Protocol))
	assert.Equal(t, clusterNLBListenPort, aws.Int64Value(lstIn.Port))
	require.Len(t, lstIn.DefaultActions, 1)
	assert.Equal(t, elbv2.ActionTypeEnumForward, aws.StringValue(lstIn.DefaultActions[0].Type))
	assert.Equal(t, out.TargetGroupArn, aws.StringValue(lstIn.DefaultActions[0].TargetGroupArn))
}

func TestEnsureClusterNLB_IdempotentReusesExisting(t *testing.T) {
	nlbp := newFakeNLBProvisioner()

	first, err := EnsureClusterNLB(nlbp, "111122223333", "alpha", []string{"subnet-aaa"}, false, nil)
	require.NoError(t, err)

	createLBCount := len(nlbp.createLBCalls)
	createTGCount := len(nlbp.createTGCalls)
	createListenerCount := len(nlbp.createListenerCalls)

	second, err := EnsureClusterNLB(nlbp, "111122223333", "alpha", []string{"subnet-aaa"}, false, nil)
	require.NoError(t, err)

	assert.Equal(t, first.LoadBalancerArn, second.LoadBalancerArn)
	assert.Equal(t, first.TargetGroupArn, second.TargetGroupArn)
	assert.Equal(t, first.ListenerArn, second.ListenerArn)
	assert.Equal(t, first.DNSName, second.DNSName)

	assert.Equal(t, createLBCount, len(nlbp.createLBCalls), "no new LB create on idempotent call")
	assert.Equal(t, createTGCount, len(nlbp.createTGCalls), "no new TG create on idempotent call")
	assert.Equal(t, createListenerCount, len(nlbp.createListenerCalls), "no new listener create on idempotent call")
}

func TestEnsureClusterNLB_LBCreateErrorSurfaced(t *testing.T) {
	nlbp := newFakeNLBProvisioner()
	nlbp.createLBErr = errors.New("InsufficientCapacity")

	_, err := EnsureClusterNLB(nlbp, "111122223333", "alpha", []string{"subnet-aaa"}, false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create NLB eks-alpha")
	assert.Empty(t, nlbp.createTGCalls, "TG create should not run when LB create fails")
}

func TestEnsureClusterNLB_InternetFacingSchemeAndPublicFrontendIP(t *testing.T) {
	nlbp := newFakeNLBProvisioner()
	// Internet-facing LB record exposes a public IpAddress in its AZ addresses.
	arn := "arn:aws:elasticloadbalancing:us-east-1:111122223333:loadbalancer/net/eks-alpha/lb-001"
	nlbp.createLBOut = &elbv2.CreateLoadBalancerOutput{
		LoadBalancers: []*elbv2.LoadBalancer{{
			LoadBalancerArn:  aws.String(arn),
			LoadBalancerName: aws.String("eks-alpha"),
			DNSName:          aws.String("eks-alpha-lb-001.us-east-1.elb.spinifex.local"),
			Type:             aws.String(elbv2.LoadBalancerTypeEnumNetwork),
			Scheme:           aws.String(elbv2.LoadBalancerSchemeEnumInternetFacing),
			AvailabilityZones: []*elbv2.AvailabilityZone{{
				LoadBalancerAddresses: []*elbv2.LoadBalancerAddress{
					{PrivateIPv4Address: aws.String("10.0.1.5")},
					{IpAddress: aws.String("203.0.113.10")},
				},
			}},
		}},
	}

	out, err := EnsureClusterNLB(nlbp, "111122223333", "alpha", []string{"subnet-aaa"}, true, nil)
	require.NoError(t, err)
	require.Len(t, nlbp.createLBCalls, 1)
	assert.Equal(t, elbv2.LoadBalancerSchemeEnumInternetFacing, aws.StringValue(nlbp.createLBCalls[0].Scheme))
	assert.Equal(t, "203.0.113.10", out.FrontendIP, "internet-facing front-end uses the public IpAddress")
}

func TestEnsureClusterNLB_InternalSchemeUsesPrivateFrontendIP(t *testing.T) {
	nlbp := newFakeNLBProvisioner()
	arn := "arn:aws:elasticloadbalancing:us-east-1:111122223333:loadbalancer/net/eks-alpha/lb-001"
	nlbp.createLBOut = &elbv2.CreateLoadBalancerOutput{
		LoadBalancers: []*elbv2.LoadBalancer{{
			LoadBalancerArn:  aws.String(arn),
			LoadBalancerName: aws.String("eks-alpha"),
			DNSName:          aws.String("eks-alpha-lb-001.us-east-1.elb.spinifex.local"),
			Type:             aws.String(elbv2.LoadBalancerTypeEnumNetwork),
			Scheme:           aws.String(elbv2.LoadBalancerSchemeEnumInternal),
			AvailabilityZones: []*elbv2.AvailabilityZone{{
				LoadBalancerAddresses: []*elbv2.LoadBalancerAddress{
					{PrivateIPv4Address: aws.String("10.0.1.5")},
				},
			}},
		}},
	}

	out, err := EnsureClusterNLB(nlbp, "111122223333", "alpha", []string{"subnet-aaa"}, false, nil)
	require.NoError(t, err)
	require.Len(t, nlbp.createLBCalls, 1)
	assert.Equal(t, elbv2.LoadBalancerSchemeEnumInternal, aws.StringValue(nlbp.createLBCalls[0].Scheme))
	assert.Equal(t, "10.0.1.5", out.FrontendIP, "internal front-end uses the private IPv4 address")
}

func TestEnsureClusterNLB_NarrowedPublicAccessSetsIngressCIDRs(t *testing.T) {
	nlbp := newFakeNLBProvisioner()

	cidrs := []string{"203.0.113.0/24", "198.51.100.7/32"}
	out, err := EnsureClusterNLB(nlbp, "111122223333", "alpha", []string{"subnet-aaa"}, true, cidrs)
	require.NoError(t, err)

	require.Len(t, nlbp.setIngressCalls, 1, "narrowed public access should drive SetLoadBalancerIngressCIDRs")
	assert.Equal(t, out.LoadBalancerArn, nlbp.setIngressCalls[0].lbArn)
	assert.Equal(t, cidrs, nlbp.setIngressCalls[0].cidrs)
}

func TestEnsureClusterNLB_DefaultPublicAccessSkipsIngressCIDRs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cidrs []string
	}{
		{"nil", nil},
		{"empty", []string{}},
		{"wide-open default", []string{defaultPublicAccessCidr}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nlbp := newFakeNLBProvisioner()
			_, err := EnsureClusterNLB(nlbp, "111122223333", "alpha", []string{"subnet-aaa"}, true, tc.cidrs)
			require.NoError(t, err)
			assert.Empty(t, nlbp.setIngressCalls, "wide-open front-end needs no ingress override")
		})
	}
}

func TestEnsureClusterNLB_InternalSkipsIngressCIDRs(t *testing.T) {
	nlbp := newFakeNLBProvisioner()

	// Even with narrowed CIDRs, an internal NLB ignores them — its ingress
	// already tracks the VPC CIDR, not a public front-end.
	_, err := EnsureClusterNLB(nlbp, "111122223333", "alpha", []string{"subnet-aaa"}, false, []string{"203.0.113.0/24"})
	require.NoError(t, err)
	assert.Empty(t, nlbp.setIngressCalls)
}

func TestEnsureClusterNLB_SetIngressErrorSurfaced(t *testing.T) {
	nlbp := newFakeNLBProvisioner()
	nlbp.setIngressErr = errors.New("InvalidLoadBalancer")

	_, err := EnsureClusterNLB(nlbp, "111122223333", "alpha", []string{"subnet-aaa"}, true, []string{"203.0.113.0/24"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "set NLB ingress CIDRs for eks-alpha")
}

func TestRegisterClusterTarget_PostsENIIPAndAPIPort(t *testing.T) {
	nlbp := newFakeNLBProvisioner()

	err := RegisterClusterTarget(nlbp, "111122223333", "arn:tg/alpha", "10.0.1.42")
	require.NoError(t, err)
	require.Len(t, nlbp.registerCalls, 1)

	in := nlbp.registerCalls[0]
	assert.Equal(t, "arn:tg/alpha", aws.StringValue(in.TargetGroupArn))
	require.Len(t, in.Targets, 1)
	assert.Equal(t, "10.0.1.42", aws.StringValue(in.Targets[0].Id))
	assert.Equal(t, k3sAPIServerPort, aws.Int64Value(in.Targets[0].Port))
}

func TestRegisterClusterTarget_EmptyInputsRejected(t *testing.T) {
	nlbp := newFakeNLBProvisioner()
	require.Error(t, RegisterClusterTarget(nlbp, "111122223333", "", "10.0.1.42"))
	require.Error(t, RegisterClusterTarget(nlbp, "111122223333", "arn:tg/alpha", ""))
	assert.Empty(t, nlbp.registerCalls)
}

func TestRegisterClusterTargets_RegistersEveryENIIPInOneCall(t *testing.T) {
	nlbp := newFakeNLBProvisioner()

	err := RegisterClusterTargets(nlbp, "111122223333", "arn:tg/alpha",
		[]string{"10.0.1.10", "10.0.2.11", "10.0.3.12"})
	require.NoError(t, err)
	require.Len(t, nlbp.registerCalls, 1)

	in := nlbp.registerCalls[0]
	assert.Equal(t, "arn:tg/alpha", aws.StringValue(in.TargetGroupArn))
	require.Len(t, in.Targets, 3)
	for i, ip := range []string{"10.0.1.10", "10.0.2.11", "10.0.3.12"} {
		assert.Equal(t, ip, aws.StringValue(in.Targets[i].Id))
		assert.Equal(t, k3sAPIServerPort, aws.Int64Value(in.Targets[i].Port))
	}
}

func TestRegisterClusterTargets_SkipsEmptyIPs(t *testing.T) {
	nlbp := newFakeNLBProvisioner()

	err := RegisterClusterTargets(nlbp, "111122223333", "arn:tg/alpha",
		[]string{"10.0.1.10", "", "10.0.3.12"})
	require.NoError(t, err)
	require.Len(t, nlbp.registerCalls, 1)
	require.Len(t, nlbp.registerCalls[0].Targets, 2)
	assert.Equal(t, "10.0.1.10", aws.StringValue(nlbp.registerCalls[0].Targets[0].Id))
	assert.Equal(t, "10.0.3.12", aws.StringValue(nlbp.registerCalls[0].Targets[1].Id))
}

func TestRegisterClusterTargets_EmptyInputsRejected(t *testing.T) {
	nlbp := newFakeNLBProvisioner()
	require.Error(t, RegisterClusterTargets(nlbp, "111122223333", "", []string{"10.0.1.10"}))
	require.Error(t, RegisterClusterTargets(nlbp, "111122223333", "arn:tg/alpha", nil))
	require.Error(t, RegisterClusterTargets(nlbp, "111122223333", "arn:tg/alpha", []string{"", ""}))
	assert.Empty(t, nlbp.registerCalls)
}

func TestRegisterClusterTargets_RegisterErrorSurfaced(t *testing.T) {
	nlbp := newFakeNLBProvisioner()
	nlbp.registerErr = errors.New("TargetGroupNotFound")

	err := RegisterClusterTargets(nlbp, "111122223333", "arn:tg/alpha", []string{"10.0.1.10"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "arn:tg/alpha")
}

func TestDeregisterClusterTarget_PostsENIIPAndAPIPort(t *testing.T) {
	nlbp := newFakeNLBProvisioner()

	err := DeregisterClusterTarget(nlbp, "111122223333", "arn:tg/alpha", "10.0.1.42")
	require.NoError(t, err)
	require.Len(t, nlbp.deregisterCalls, 1)

	in := nlbp.deregisterCalls[0]
	assert.Equal(t, "arn:tg/alpha", aws.StringValue(in.TargetGroupArn))
	require.Len(t, in.Targets, 1)
	assert.Equal(t, "10.0.1.42", aws.StringValue(in.Targets[0].Id))
	assert.Equal(t, k3sAPIServerPort, aws.Int64Value(in.Targets[0].Port))
}

func TestDeleteClusterNLB_DeletesBoth(t *testing.T) {
	nlbp := newFakeNLBProvisioner()
	out, err := EnsureClusterNLB(nlbp, "111122223333", "alpha", []string{"subnet-aaa"}, false, nil)
	require.NoError(t, err)

	require.NoError(t, DeleteClusterNLB(nlbp, "111122223333", "alpha"))
	require.Len(t, nlbp.deleteLBCalls, 1)
	assert.Equal(t, out.LoadBalancerArn, aws.StringValue(nlbp.deleteLBCalls[0].LoadBalancerArn))
	require.Len(t, nlbp.deleteTGCalls, 1)
	assert.Equal(t, out.TargetGroupArn, aws.StringValue(nlbp.deleteTGCalls[0].TargetGroupArn))
}

func TestDeleteClusterNLB_MissingResourcesNoOp(t *testing.T) {
	nlbp := newFakeNLBProvisioner()

	require.NoError(t, DeleteClusterNLB(nlbp, "111122223333", "alpha"))
	assert.Empty(t, nlbp.deleteLBCalls)
	assert.Empty(t, nlbp.deleteTGCalls)
}

func TestDeleteClusterNLB_FirstErrorSurfacedSweepContinues(t *testing.T) {
	nlbp := newFakeNLBProvisioner()
	_, err := EnsureClusterNLB(nlbp, "111122223333", "alpha", []string{"subnet-aaa"}, false, nil)
	require.NoError(t, err)
	nlbp.deleteLBErr = errors.New("LoadBalancerInUse")

	err = DeleteClusterNLB(nlbp, "111122223333", "alpha")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete NLB eks-alpha")
	assert.Len(t, nlbp.deleteLBCalls, 1)
	assert.Len(t, nlbp.deleteTGCalls, 1, "TG delete should still be attempted after LB delete fails")
}

func assertELBv2TaggedAsEKS(t *testing.T, tgs []*elbv2.Tag, clusterName string) {
	t.Helper()
	got := map[string]string{}
	for _, tg := range tgs {
		if tg == nil || tg.Key == nil || tg.Value == nil {
			continue
		}
		got[*tg.Key] = *tg.Value
	}
	assert.Equal(t, tags.ManagedByEKS, got[tags.ManagedByKey])
	assert.Equal(t, clusterName, got[clusterEKSClusterTagKey])
}
