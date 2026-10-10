package layering

import (
	"slices"
	"strings"
)

type layer string

const (
	layerFoundation layer = "foundation"
	layerContracts  layer = "contracts"
	layerDomain     layer = "domains"
	layerIngress    layer = "ingress"
	layerRuntime    layer = "runtime"
	layerOperator   layer = "operator"
	layerAgents     layer = "agents"
	layerProviders  layer = "providers"
	layerBootstrap  layer = "bootstrap"
	layerCmd        layer = "cmd"
	layerInternal   layer = "internal"
	layerTestkit    layer = "testkit"
	layerLegacy     layer = "legacy"
	// layerTests is valid only as an import target; tests/ is never scanned.
	layerTests layer = "tests"
)

// domainKind distinguishes the parts of a domain the rules treat differently.
type domainKind int

const (
	notDomain     domainKind = iota
	domainImpl               // domains/<d>/... resource implementation
	domainAWSAPI             // domains/<d>/awsapi/... AWS action adapters
	domainProj               // domains/<d>/projection/... authorised view
	legacyImpl               // handlers/<d>/... legacy domain implementation
	legacyAdapter            // gateway/<d>/... legacy AWS action adapters
)

type class struct {
	layer  layer
	legacy string // legacy root name when layer is layerLegacy
	domain string // owning domain for domain-shaped and agent packages
	kind   domainKind
}

func (c class) domainShaped() bool { return c.kind != notDomain }

// isLegacy reports whether c sits under one of the named legacy roots.
func (c class) isLegacy(roots ...string) bool {
	if c.layer != layerLegacy {
		return false
	}
	if len(roots) == 0 {
		return true
	}
	return slices.Contains(roots, c.legacy)
}

// legacyRoots are pre-ADR-0001 application roots still standing.
var legacyRoots = map[string]bool{
	"handlers": true, "gateway": true, "daemon": true,
	"vpcd": true, "admin": true, "accountteardown": true, "lbagent": true,
}

// gatewayDomains names the domain each gateway/<service> adapter belongs to;
// Bedrock is the AWS surface of the Ochre domain.
var gatewayDomains = map[string]string{
	"bedrock": "ochre", "ecs": "ecs", "eks": "eks", "elbv2": "elbv2", "iam": "iam",
	"rds": "rds", "sts": "sts", "tagging": "tagging",
}

// runtimeLegacy and operatorLegacy are the legacy roots whose ADR-0001 target
// is runtime/ and operator/ respectively.
var (
	runtimeLegacy  = []string{"daemon", "vpcd"}
	operatorLegacy = []string{"admin", "accountteardown"}
)

// classify maps a module-relative package directory to its layer. ok is false
// for a package that has no deliberate classification.
func classify(full string) (class, bool) {
	parts := strings.Split(full, "/")
	switch parts[0] {
	case "cmd":
		return class{layer: layerCmd}, len(parts) >= 2
	case "contracts":
		return class{layer: layerContracts}, len(parts) >= 2
	case "internal":
		if len(parts) < 2 {
			return class{}, false
		}
		if parts[1] == "testkit" || parts[1] == "tooling" {
			return class{layer: layerTestkit}, true
		}
		return class{layer: layerInternal}, true
	case "tests":
		return class{layer: layerTests}, true
	case "spinifex":
		return classifyApp(parts[1:])
	}
	return class{}, false
}

func classifyApp(app []string) (class, bool) {
	if len(app) == 0 {
		return class{}, false
	}
	switch app[0] {
	case "foundation":
		return class{layer: layerFoundation}, true
	case "ingress":
		return class{layer: layerIngress}, true
	case "runtime":
		return class{layer: layerRuntime}, true
	case "operator":
		return class{layer: layerOperator}, true
	case "providers":
		return class{layer: layerProviders}, true
	case "bootstrap":
		return class{layer: layerBootstrap}, true
	case "agents":
		c := class{layer: layerAgents}
		if len(app) >= 2 {
			c.domain = app[1]
		}
		return c, true
	case "domains":
		if len(app) < 2 {
			return class{}, false
		}
		c := class{layer: layerDomain, domain: app[1], kind: domainImpl}
		if len(app) >= 3 {
			switch app[2] {
			case "awsapi":
				c.kind = domainAWSAPI
			case "projection":
				c.kind = domainProj
			}
		}
		return c, true
	}
	if !legacyRoots[app[0]] {
		return class{}, false
	}
	c := class{layer: layerLegacy, legacy: app[0]}
	switch {
	case app[0] == "handlers" && len(app) >= 2:
		c.domain, c.kind = app[1], legacyImpl
	case app[0] == "gateway" && len(app) >= 2 && gatewayDomains[app[1]] != "":
		c.domain, c.kind = gatewayDomains[app[1]], legacyAdapter
	}
	return c, true
}

// fullPath reverses shortName for classification.
func fullPath(name string) string {
	top, _, _ := strings.Cut(name, "/")
	switch top {
	case "cmd", "contracts", "internal", "tests", "docs":
		return name
	}
	return "spinifex/" + name
}
