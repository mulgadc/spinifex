package awsmodel

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// RequestOptions places generated ARNs in the account and region the
// requests are sent as. AccessKeyID is the key the requests are signed with.
// Fixtures maps a member to an existing resource it can name.
type RequestOptions struct {
	AccountID   string
	Region      string
	AccessKeyID string
	Fixtures    map[string]string
}

// Models constrain most identifiers only by length, so "spxa" is a model-valid
// AMI ID that AWS rejects. Hints give resource-naming members a value of the
// right form, used only if the member's own constraints accept it.

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

// conditionallyRequired names members the model marks optional that the
// operation still needs in its base request. The services refuse their absence
// with a code the judge cannot tell from a real finding.
var conditionallyRequired = map[Service]map[string][]string{
	EC2: {"CreateCapacityReservation": {"AvailabilityZone"}},
	ECS: {
		"CreateCapacityProvider": {"autoScalingGroupProvider"},
		"CreateService":          {"taskDefinition"},
	},
	RDS: {
		"CreateDBInstance":                {"AllocatedStorage", "MasterUsername", "MasterUserPassword"},
		"RestoreDBInstanceFromDBSnapshot": {"DBSnapshotIdentifier"},
	},
}

const (
	// imageManifest is well formed but names blobs no repository holds.
	imageManifest    = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":2,"digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"},"layers":[]}`
	lifecyclePolicy  = `{"rules":[{"rulePriority":1,"description":"expire untagged","selection":{"tagStatus":"untagged","countType":"sinceImagePushed","countUnit":"days","countNumber":14},"action":{"type":"expire"}}]}`
	repositoryPolicy = `{"Version":"2012-10-17","Statement":[{"Sid":"pull","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::%s:root"},"Action":"ecr:BatchGetImage"}]}`

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
	if fixture, ok := options.Fixtures[member]; ok {
		return fixture, true
	}
	suffix := fmt.Sprintf("%017x", g.saltNumber)
	name := "spx" + g.salt

	switch g.model.service {
	case EC2:
		switch member {
		case "AvailabilityZone":
			return options.Region + "a", true
		case "SourceRegion":
			return options.Region, true
		}
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
		case "ProtocolVersion":
			return "HTTP1", true
		case "HttpCode":
			return "200", true
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
		case "MasterUserPassword":
			return "spx-conformance-" + g.salt, true
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
	case ACM:
		// The integration tenant CA permits example.com.
		switch member {
		case "DomainName", "SubjectAlternativeNames", "ValidationDomain":
			return name + ".example.com", true
		}
	case ECR:
		switch member {
		case "resourceArn":
			return fmt.Sprintf("arn:aws:ecr:%s:%s:repository/%s", options.Region, options.AccountID, name), true
		case "imageManifest":
			return imageManifest, true
		case "lifecyclePolicyText":
			return lifecyclePolicy, true
		case "policyText":
			return fmt.Sprintf(repositoryPolicy, options.AccountID), true
		}
	case EKS:
		switch member {
		case "resourceArn":
			return fmt.Sprintf("arn:aws:eks:%s:%s:cluster/%s", options.Region, options.AccountID, name), true
		case "policyArn":
			return "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy", true
		case "addonName":
			return "aws-ebs-csi-driver", true
		}
		// An access entry's type; a nested type names an identity provider's.
		if member == "type" && len(path) == 1 {
			return "STANDARD", true
		}
	case STS:
		// GetAccessKeyInfo answers only for a key that exists.
		if member == "AccessKeyId" && options.AccessKeyID != "" {
			return options.AccessKeyID, true
		}
	}

	switch member {
	case "PolicyArn", "PermissionsBoundary":
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

// integerHint returns a value AWS accepts for an integer member whose range
// the model leaves out, if one is known.
func (g *requestGenerator) integerHint(path []pathStep) (int64, bool) {
	if g.model.service == RDS && memberName(path) == "AllocatedStorage" {
		// The smallest gp2/gp3 allocation PostgreSQL, the Engine hint, accepts.
		return 20, true
	}
	return 0, false
}

// blobHint returns a value AWS accepts for a blob member, if one is known.
func (g *requestGenerator) blobHint(path []pathStep) ([]byte, bool) {
	if g.model.service != ACM {
		return nil, false
	}
	// GenerateRequests has already surfaced a generation error.
	material, err := importMaterial()
	if err != nil {
		return nil, false
	}
	switch memberName(path) {
	case "Certificate":
		return material.certificate, true
	case "PrivateKey":
		return material.privateKey, true
	case "CertificateChain":
		return material.chain, true
	}
	return nil, false
}

// certificateMaterial is a PEM leaf certificate, its key and the CA that
// signed it, as ImportCertificate takes them.
type certificateMaterial struct {
	certificate, privateKey, chain []byte
}

// importMaterial is generated once per run; minting a key per request would
// dominate the ACM cases.
var importMaterial = sync.OnceValues(func() (certificateMaterial, error) {
	now := time.Now().UTC()
	caKey, caDER, err := newCertificate(&x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Spinifex Conformance CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}, nil, nil)
	if err != nil {
		return certificateMaterial{}, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("awsmodel: parse conformance CA: %w", err)
	}
	leafKey, leafDER, err := newCertificate(&x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "spx.example.com"},
		DNSNames:     []string{"spx.example.com"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, caKey)
	if err != nil {
		return certificateMaterial{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("awsmodel: marshal conformance key: %w", err)
	}
	return certificateMaterial{
		certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		privateKey:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		chain:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
	}, nil
})

// newCertificate creates a P-256 key and a certificate for it from template,
// signed by parent's key, or self-signed when parent is nil.
func newCertificate(template, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("awsmodel: generate conformance key: %w", err)
	}
	if parent == nil {
		parent, parentKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
	if err != nil {
		return nil, nil, fmt.Errorf("awsmodel: create conformance certificate: %w", err)
	}
	return key, der, nil
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
