package arn

import (
	"fmt"
	"strings"
)

// ELBv2ResourceType is the resource-type segment of an
// arn:aws:elasticloadbalancing ARN.
type ELBv2ResourceType string

const (
	ELBv2LoadBalancer ELBv2ResourceType = "loadbalancer"
	ELBv2TargetGroup  ELBv2ResourceType = "targetgroup"
	ELBv2Listener     ELBv2ResourceType = "listener"
	ELBv2ListenerRule ELBv2ResourceType = "listener-rule"
)

// elbv2NetworkType is the load balancer type ELBv2 spells "network". This
// package is a leaf, so the value is repeated here rather than imported.
const elbv2NetworkType = "network"

// ELBv2LBPathSegment returns the path segment a load balancer type takes inside
// an ARN: "net" for a network load balancer, "app" for everything else.
func ELBv2LBPathSegment(lbType string) string {
	if lbType == elbv2NetworkType {
		return "net"
	}
	return "app"
}

// FormatELBv2LoadBalancer builds
// arn:aws:elasticloadbalancing:<region>:<account>:loadbalancer/<app|net>/<name>/<id>.
func FormatELBv2LoadBalancer(region, accountID, name, lbID, lbType string) string {
	return fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:%s/%s/%s/%s",
		region, accountID, ELBv2LoadBalancer, ELBv2LBPathSegment(lbType), name, lbID)
}

// FormatELBv2TargetGroup builds
// arn:aws:elasticloadbalancing:<region>:<account>:targetgroup/<name>/<id>.
func FormatELBv2TargetGroup(region, accountID, name, tgID string) string {
	return fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:%s/%s/%s",
		region, accountID, ELBv2TargetGroup, name, tgID)
}

// FormatELBv2Listener builds
// arn:aws:elasticloadbalancing:<region>:<account>:listener/<app|net>/<lb>/<lbID>/<id>.
func FormatELBv2Listener(region, accountID, lbName, lbID, listenerID, lbType string) string {
	return fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:%s/%s/%s/%s/%s",
		region, accountID, ELBv2Listener, ELBv2LBPathSegment(lbType), lbName, lbID, listenerID)
}

// FormatELBv2Resource builds an ELBv2 ARN from an already-formed resource
// component, such as one derived from a caller-supplied parent ARN.
func FormatELBv2Resource(region, accountID string, kind ELBv2ResourceType, resource string) string {
	return fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:%s/%s", region, accountID, kind, resource)
}

// ParsedELBv2 is an arn:aws:elasticloadbalancing ARN split into its parts.
// Resource is the path following the resource-type segment, which for every
// ELBv2 type but the target group carries the load balancer's own path.
type ParsedELBv2 struct {
	Region    string
	AccountID string
	Kind      ELBv2ResourceType
	Resource  string
}

// ParseELBv2 splits an ELBv2 ARN. ok is false for anything that is not an
// arn:aws:elasticloadbalancing ARN naming one of the four resource types: an
// ARN this stack never builds has no resource it can correctly stand for.
func ParseELBv2(value string) (ParsedELBv2, bool) {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "elasticloadbalancing" {
		return ParsedELBv2{}, false
	}
	rawKind, resource, found := strings.Cut(parts[5], "/")
	if !found || rawKind == "" {
		return ParsedELBv2{}, false
	}
	kind := ELBv2ResourceType(rawKind)
	switch kind {
	case ELBv2LoadBalancer, ELBv2TargetGroup, ELBv2Listener, ELBv2ListenerRule:
		return ParsedELBv2{Region: parts[3], AccountID: parts[4], Kind: kind, Resource: resource}, true
	default:
		return ParsedELBv2{}, false
	}
}
