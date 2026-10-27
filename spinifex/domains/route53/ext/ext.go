// Package ext holds the wire types of the Spinifex extensions to the Route 53
// API: the activation a hosted zone needs before it is served, which AWS has
// no equivalent of. Standard SDKs never call them.
package ext

import "time"

// Actions served on the route53 signing service beside the AWS model's.
const (
	ActionGetHostedZoneActivation    = "GetHostedZoneActivation"
	ActionVerifyHostedZoneDelegation = "VerifyHostedZoneDelegation"
)

// GetHostedZoneActivationInput names the zone whose activation to report.
type GetHostedZoneActivationInput struct {
	_  struct{} `locationName:"GetHostedZoneActivationRequest" type:"structure"`
	Id *string  `location:"uri" locationName:"Id" type:"string" required:"true"`
}

// VerifyHostedZoneDelegationInput names the zone to check now.
type VerifyHostedZoneDelegationInput struct {
	_  struct{} `locationName:"VerifyHostedZoneDelegationRequest" type:"structure"`
	Id *string  `location:"uri" locationName:"Id" type:"string" required:"true"`
}

// Activation is whether northstar serves a zone, and what the last check of
// its delegation found.
type Activation struct {
	_ struct{} `type:"structure"`

	// State is PENDING_NAMESERVERS, ACTIVE, NAMESERVERS_LOST or SUSPENDED.
	State  *string `type:"string"`
	Reason *string `type:"string"`
	// NameServers are the servers the parent must delegate the zone to.
	NameServers []*string  `locationNameList:"NameServer" type:"list"`
	CheckedAt   *time.Time `type:"timestamp"`
	// DelegatedTo is the NS set the parent published at the last check, and
	// DelegationSource the parent zone or server that answered.
	DelegatedTo      []*string `locationNameList:"NameServer" type:"list"`
	DelegationSource *string   `type:"string"`
	// Override is set when an operator activated or suspended the zone.
	Override *string `type:"string"`
}

// GetHostedZoneActivationOutput reports a zone's activation.
type GetHostedZoneActivationOutput struct {
	_            struct{}    `type:"structure"`
	HostedZoneId *string     `type:"string"`
	Activation   *Activation `type:"structure"`
}

// VerifyHostedZoneDelegationOutput reports the activation the check left.
type VerifyHostedZoneDelegationOutput struct {
	_            struct{}    `type:"structure"`
	HostedZoneId *string     `type:"string"`
	Activation   *Activation `type:"structure"`
}
