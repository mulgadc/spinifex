package layering

import "slices"

// rule is one ADR-0001 dependency rule over a classified import edge.
type rule struct {
	id     string // subtest name, led by the ADR-0001 clause it encodes
	clause string // the ADR text the rule enforces, quoted in failures
	hint   string // one-line fix direction
	// subject reports whether an importer is governed by the rule; the
	// non-empty guard requires at least one governed package per rule.
	subject func(from class, fromName string) bool
	// forbids reports whether the rule rejects the edge.
	forbids func(from, to class, fromName string) bool
}

// awsgwRole is the composition root ADR-0002 lets wire domain registrations.
const awsgwRole = "runtime/roles/awsgw"

// qualificationBinaries are the developer/qualification mains ADR-0001 lets
// keep reusable source in internal/testkit or internal/tooling.
var qualificationBinaries = map[string]bool{
	"cmd/aws-model-coverage": true, "cmd/dhcptest": true, "cmd/spx-loadgen": true,
}

func isLayer(c class, layers ...layer) bool {
	return slices.Contains(layers, c.layer)
}

// importsDomainImplementation is any domain-owned package other than a named
// projection, in its target or legacy home.
func importsDomainImplementation(to class) bool {
	return to.domainShaped() && to.kind != domainProj
}

var rules = []rule{
	{
		id:      "R1_FoundationImportsOnlyFoundation",
		clause:  "rule 1: foundation imports neither domains, ingress, runtime nor operator (enforced conservatively: foundation imports no module package above foundation)",
		hint:    "move the dependency's needed primitive into foundation or invert it through a parameter",
		subject: func(f class, _ string) bool { return f.layer == layerFoundation },
		forbids: func(_, t class, _ string) bool { return t.layer != layerFoundation },
	},
	{
		id:      "R2_IngressImportsNoImplementation",
		clause:  "rule 2: ingress/aws imports no domain implementation; domains supply their own action registration through the ingress registration contract",
		hint:    "take the capability through the registration contract wired by runtime/roles/awsgw",
		subject: func(f class, _ string) bool { return f.layer == layerIngress },
		forbids: func(_, t class, _ string) bool {
			return !isLayer(t, layerFoundation, layerContracts, layerIngress)
		},
	},
	{
		id:      "R3_DomainImportsNoOtherDomainImplementation",
		clause:  "rule 3: a domain does not read another domain's private records, migrations or repository; it uses a consumer-owned capability, a named projection, or a versioned contracts/<domain>/v1 contract",
		hint:    "declare a narrow capability in the consumer, or import the owner's projection or contracts/<domain>/v1",
		subject: func(f class, _ string) bool { return f.domainShaped() },
		forbids: func(f, t class, _ string) bool {
			return f.domainShaped() && importsDomainImplementation(t) && t.domain != f.domain
		},
	},
	{
		id:      "R4_ProviderImportsNoDomainImplementation",
		clause:  "rule 4: a provider adapter implements a provider contract and cannot acquire AWS-visible resource authority by reading a domain's private state",
		hint:    "pass the needed values into the provider contract instead of importing the domain",
		subject: func(f class, _ string) bool { return f.layer == layerProviders },
		forbids: func(_, t class, _ string) bool { return t.domainShaped() || t.isLegacy() },
	},
	{
		id:      "R5_DomainImportsNoRuntimeOrOperator",
		clause:  "rule 5: operator and runtime are composition consumers; they wire domain modules, so a domain does not import them",
		hint:    "expose the runtime or operator state through a domain-owned projection or a capability wired at the composition root",
		subject: func(f class, _ string) bool { return f.domainShaped() },
		forbids: func(f, t class, _ string) bool {
			return f.domainShaped() && (isLayer(t, layerRuntime, layerOperator) ||
				t.isLegacy(runtimeLegacy...) || t.isLegacy(operatorLegacy...))
		},
	},
	{
		id:      "R5_AWSAPIAdaptersImportedOnlyByOwnerOrComposition",
		clause:  "rule 5: operator and runtime call public use cases; a domain's awsapi action adapters are owned by that domain and wired only by the composition root",
		hint:    "call an owner-provided capability instead of the AWS action adapter",
		subject: func(_ class, _ string) bool { return true },
		forbids: func(f, t class, fromName string) bool {
			if t.kind != domainAWSAPI || fromName == awsgwRole {
				return false
			}
			return !f.domainShaped() || f.domain != t.domain
		},
	},
	{
		id:      "R5_OperatorImportsNoLegacyImplementation",
		clause:  "rule 5 and the cmd/spinifex map row: operator use cases reach domains through public capabilities and do not read domain-private state",
		hint:    "reach the domain through its public capability; do not import a legacy implementation root",
		subject: func(f class, _ string) bool { return f.layer == layerOperator },
		forbids: func(f, t class, _ string) bool { return f.layer == layerOperator && t.isLegacy() },
	},
	{
		id:      "R6_ProductionImportsNoTestkit",
		clause:  "rule 6: test support is under internal/testkit; production packages do not import it",
		hint:    "move the shared code to its production owner or keep the import in a _test.go file",
		subject: func(f class, n string) bool { return f.layer != layerTestkit && !qualificationBinaries[n] },
		forbids: func(f, t class, n string) bool {
			return f.layer != layerTestkit && !qualificationBinaries[n] && isLayer(t, layerTestkit, layerTests)
		},
	},
	{
		id:      "Contracts_ImportOnlyFoundationAndContracts",
		clause:  "decision item 1 and the area table: contracts own cross-process vocabulary, not application interfaces, domain persistence or transport implementation",
		hint:    "keep wire types self-contained; depend only on foundation or another contract",
		subject: func(f class, _ string) bool { return f.layer == layerContracts },
		forbids: func(_, t class, _ string) bool { return !isLayer(t, layerFoundation, layerContracts) },
	},
	{
		id:      "Domains_ImportNoAgentsIngressOrLegacy",
		clause:  "area table and domain module shape: a domain owns no other domain's implementation; only its awsapi adapters may use the generic ingress/aws machinery",
		hint:    "depend on a contract or capability; leave ingress use to the domain's awsapi package",
		subject: func(f class, _ string) bool { return f.domainShaped() },
		forbids: func(f, t class, _ string) bool {
			if !f.domainShaped() {
				return false
			}
			switch {
			case t.layer == layerAgents || t.isLegacy("lbagent", "utils"):
				return true
			case t.layer == layerIngress:
				return f.kind != domainAWSAPI && f.kind != legacyAdapter
			case t.isLegacy("gateway") && !t.domainShaped():
				return true
			case t.isLegacy("handlers", "gateway"):
				return f.layer == layerDomain
			}
			return false
		},
	},
	{
		id:      "Agents_ImportNoDomainOrLegacyImplementation",
		clause:  "area table: agents/<domain> own in-guest and service-role behaviour, not tenant API authority or another domain's private state",
		hint:    "move the shared wire vocabulary behind a guest/controller contract under contracts/",
		subject: func(f class, _ string) bool { return f.layer == layerAgents || f.isLegacy("lbagent") },
		forbids: func(f, t class, _ string) bool {
			return (f.layer == layerAgents || f.isLegacy("lbagent")) && (t.domainShaped() || t.isLegacy())
		},
	},
	{
		id:      "Q22_STSImportsNoEKSImplementation",
		clause:  "Q-22 prohibits the STS-to-EKS dependency in its present form; cut it through an identity-owned contract or an EKS projection",
		hint:    "consume an EKS projection or an identity-owned contract instead of the EKS implementation",
		subject: func(f class, _ string) bool { return f.domainShaped() && f.domain == "sts" },
		forbids: func(f, t class, _ string) bool {
			return f.domainShaped() && f.domain == "sts" && t.domain == "eks" &&
				(importsDomainImplementation(t) || t.layer == layerAgents)
		},
	},
	{
		id:      "Interim_DaemonGatewayHandlersDirection",
		clause:  "during migration the existing daemon -> gateway -> handlers rule remains the guardrail (Q-20, Q-29)",
		hint:    "keep the dependency pointing daemon -> gateway -> handlers; invert it through a callback or capability",
		subject: func(f class, _ string) bool { return f.isLegacy("handlers", "gateway") },
		forbids: func(f, t class, _ string) bool {
			switch {
			case f.isLegacy("handlers"):
				return t.isLegacy("gateway", "daemon")
			case f.isLegacy("gateway"):
				return t.isLegacy("daemon")
			}
			return false
		},
	},
}
