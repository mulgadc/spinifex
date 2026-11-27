package elbv2

import (
	"cmp"
	"errors"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	resourcearn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
)

// fixtureMaxTargets bounds the target tuples one OVN fixture owner keeps.
const fixtureMaxTargets = 16

// fixtureEditAttempts bounds the edits retried after a definite CAS conflict.
const fixtureEditAttempts = 5

var (
	// errFixtureNotSealed reports an edit of an owner whose root is still pending.
	errFixtureNotSealed = errors.New("elbv2: load balancer root is not sealed")
	// errFixtureEditUncertain reports an edit whose write outcome is unknown and
	// whose re-read does not show the intended snapshot. A caller may retry it.
	errFixtureEditUncertain = errors.New("elbv2: load balancer edit outcome is unknown")
)

// fixtureListener is the desired listener and target set of a sealed OVN
// owner. The owner revision that holds it identifies the whole version.
type fixtureListener struct {
	ListenerArn    string          `json:"listenerArn"`
	Port           int64           `json:"port"`
	TargetGroupArn string          `json:"targetGroupArn"`
	TargetPort     int64           `json:"targetPort"`
	Targets        []fixtureTarget `json:"targets,omitempty"`
}

// fixtureTarget is one desired instance and the port it receives traffic on.
type fixtureTarget struct {
	InstanceID string `json:"instanceId"`
	Port       int64  `json:"port"`
}

func compareFixtureTargets(a, b fixtureTarget) int {
	return cmp.Or(cmp.Compare(a.InstanceID, b.InstanceID), cmp.Compare(a.Port, b.Port))
}

func validFixturePort(port int64) bool {
	return port >= 1 && port <= 65535
}

// check reports the first way l breaks the owner invariants for root: the
// listener ARN must name a listener of root, and targets must be unique,
// sorted and within the fixture limit.
func (l *fixtureListener) check(root *LoadBalancerRecord) error {
	rootARN, ok := resourcearn.ParseELBv2(root.LoadBalancerArn)
	if !ok {
		return errors.New("root ARN is not an ELBv2 ARN")
	}
	listener, ok := resourcearn.ParseELBv2(l.ListenerArn)
	prefix := rootARN.Resource + "/"
	if !ok || listener.Kind != resourcearn.ELBv2Listener || listener.Region != rootARN.Region ||
		listener.AccountID != rootARN.AccountID || !strings.HasPrefix(listener.Resource, prefix) ||
		strings.Contains(listener.Resource[len(prefix):], "/") || len(listener.Resource) == len(prefix) {
		return fmt.Errorf("listener ARN %q is not a listener of %s", l.ListenerArn, root.LoadBalancerArn)
	}
	group, ok := resourcearn.ParseELBv2(l.TargetGroupArn)
	if !ok || group.Kind != resourcearn.ELBv2TargetGroup || group.Region != rootARN.Region ||
		group.AccountID != rootARN.AccountID {
		return fmt.Errorf("target group ARN %q is outside the root's account and region", l.TargetGroupArn)
	}
	if !validFixturePort(l.Port) || !validFixturePort(l.TargetPort) {
		return fmt.Errorf("listener port %d or target port %d is out of range", l.Port, l.TargetPort)
	}
	if len(l.Targets) > fixtureMaxTargets {
		return fmt.Errorf("%d targets exceed the limit of %d", len(l.Targets), fixtureMaxTargets)
	}
	for i, t := range l.Targets {
		if !projection.ValidInstanceID(t.InstanceID) || !validFixturePort(t.Port) {
			return fmt.Errorf("target %q port %d is not an instance tuple", t.InstanceID, t.Port)
		}
		if i > 0 && compareFixtureTargets(l.Targets[i-1], t) >= 0 {
			return fmt.Errorf("target %q port %d is duplicated or out of order", t.InstanceID, t.Port)
		}
	}
	return nil
}
