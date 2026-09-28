package awsmodel

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// RequestOptions places generated ARNs in the account and region the
// requests are sent as.
type RequestOptions struct {
	AccountID string
	Region    string
}

// Models constrain most identifiers only by length, so a value valid by the
// model alone, such as "spxa" for an AMI ID, is one AWS rejects too. These
// hints give members that name a resource a value of the right form; a hint
// is used only if the member's own constraints also accept it.

// ec2IDPrefixes maps the resource an EC2 "<Resource>Id" member names to its
// ID prefix.
var ec2IDPrefixes = map[string]string{
	"Allocation":                "eipalloc",
	"CapacityReservation":       "cr",
	"CarrierGateway":            "cagw",
	"DhcpOptions":               "dopt",
	"EgressOnlyInternetGateway": "eigw",
	"Fleet":                     "fleet",
	"Group":                     "sg",
	"Host":                      "h",
	"Image":                     "ami",
	"Instance":                  "i",
	"InternetGateway":           "igw",
	"KeyPair":                   "key",
	"LaunchTemplate":            "lt",
	"NatGateway":                "nat",
	"NetworkAcl":                "acl",
	"NetworkInterface":          "eni",
	"PlacementGroup":            "pg",
	"PrefixList":                "pl",
	"RouteTable":                "rtb",
	"SecurityGroup":             "sg",
	"SecurityGroupRule":         "sgr",
	"Snapshot":                  "snap",
	"SourceImage":               "ami",
	"SourceSnapshot":            "snap",
	"SpotInstanceRequest":       "sir",
	"Subnet":                    "subnet",
	"TransitGateway":            "tgw",
	"Volume":                    "vol",
	"Vpc":                       "vpc",
	"VpcEndpoint":               "vpce",
	"VpcPeeringConnection":      "pcx",
}

var ec2IDMember = regexp.MustCompile(`^(.*?)Ids?$`)

const (
	policyDocument      = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`
	trustPolicyDocument = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
)

// hint returns a value of the right form for the member at path, if one is
// known. salt keeps names distinct between request cases.
func (g *requestGenerator) hint(path []pathStep) (string, bool) {
	member := memberName(path)
	if member == "" {
		return "", false
	}
	options := g.options
	suffix := fmt.Sprintf("%017x", g.saltNumber)
	name := "spx" + g.salt

	switch g.model.service {
	case EC2:
		if match := ec2IDMember.FindStringSubmatch(capitalize(member)); match != nil {
			if prefix, ok := ec2IDPrefixes[match[1]]; ok {
				return prefix + "-" + suffix, true
			}
		}
	case ElasticLoadBalancingV2:
		base := fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:", options.Region, options.AccountID)
		switch member {
		case "LoadBalancerArn", "LoadBalancerArns", "ResourceArns":
			return base + "loadbalancer/app/" + name + "/" + suffix[1:], true
		case "TargetGroupArn", "TargetGroupArns":
			return base + "targetgroup/" + name + "/" + suffix[1:], true
		case "ListenerArn", "ListenerArns":
			return base + "listener/app/" + name + "/" + suffix[1:] + "/" + suffix[1:], true
		case "RuleArn", "RuleArns":
			return base + "listener-rule/app/" + name + "/" + suffix[1:] + "/" + suffix[1:] + "/" + suffix[1:], true
		}
	case RDS:
		switch member {
		case "ResourceName":
			return fmt.Sprintf("arn:aws:rds:%s:%s:db:%s", options.Region, options.AccountID, name), true
		case "Engine":
			return "postgres", true
		case "DBInstanceClass":
			return "db.t3.medium", true
		case "DBParameterGroupFamily":
			return "postgres18", true
		case "SnapshotType":
			return "manual", true
		case "Source":
			return "user", true
		}
	case ECS:
		switch member {
		case "resourceArn":
			return fmt.Sprintf("arn:aws:ecs:%s:%s:cluster/%s", options.Region, options.AccountID, name), true
		case "taskDefinition":
			return name + ":1", true
		}
	}

	switch member {
	case "PolicyArn", "PermissionsBoundary", "arn":
		return fmt.Sprintf("arn:aws:iam::%s:policy/%s", options.AccountID, name), true
	case "RoleArn":
		return fmt.Sprintf("arn:aws:iam::%s:role/%s", options.AccountID, name), true
	case "CertificateArn":
		return fmt.Sprintf("arn:aws:acm:%s:%s:certificate/%s-0000-0000-0000-000000000000", options.Region, options.AccountID, suffix[9:]), true
	case "PolicyDocument", "Policy":
		return policyDocument, true
	case "AssumeRolePolicyDocument":
		return trustPolicyDocument, true
	case "Url":
		return "https://" + name + ".example.com", true
	case "AccessKeyId":
		return "AKIA" + strings.ToUpper(fmt.Sprintf("%016x", g.saltNumber)), true
	}
	return "", false
}

// memberName is the structure member a value at path belongs to, looking
// through list indices.
func memberName(path []pathStep) string {
	for _, step := range slices.Backward(path) {
		if name, ok := step.(string); ok {
			return name
		}
	}
	return ""
}

func capitalize(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
