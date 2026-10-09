package layering

// allowance is one recorded forbidden import, allowed until its debt clears.
type allowance struct {
	From, To string
	Debt     string // subject of the migration record's debt entry or close-out row
	Reason   string
}

// allowlist mirrors the dependency-rule debt and close-out table in
// docs/PACKAGE_BOUNDARY_MIGRATION.md. It only shrinks: add an edge only with
// recorded debt, and delete an entry as soon as its import is gone.
var allowlist = []allowance{
	{From: "accountteardown", To: "domains/ec2/awsapi/eip", Debt: "accountteardown (dependency)", Reason: "EC2 reapers call the awsapi action adapters; needs domain-owned cleanup capabilities"},
	{From: "accountteardown", To: "domains/ec2/awsapi/igw", Debt: "accountteardown (dependency)", Reason: "EC2 reapers call the awsapi action adapters; needs domain-owned cleanup capabilities"},
	{From: "accountteardown", To: "domains/ec2/awsapi/image", Debt: "accountteardown (dependency)", Reason: "EC2 reapers call the awsapi action adapters; needs domain-owned cleanup capabilities"},
	{From: "accountteardown", To: "domains/ec2/awsapi/instance", Debt: "accountteardown (dependency)", Reason: "EC2 reapers call the awsapi action adapters; needs domain-owned cleanup capabilities"},
	{From: "accountteardown", To: "domains/ec2/awsapi/key", Debt: "accountteardown (dependency)", Reason: "EC2 reapers call the awsapi action adapters; needs domain-owned cleanup capabilities"},
	{From: "accountteardown", To: "domains/ec2/awsapi/launchtemplate", Debt: "accountteardown (dependency)", Reason: "EC2 reapers call the awsapi action adapters; needs domain-owned cleanup capabilities"},
	{From: "accountteardown", To: "domains/ec2/awsapi/placementgroup", Debt: "accountteardown (dependency)", Reason: "EC2 reapers call the awsapi action adapters; needs domain-owned cleanup capabilities"},
	{From: "accountteardown", To: "domains/ec2/awsapi/routetable", Debt: "accountteardown (dependency)", Reason: "EC2 reapers call the awsapi action adapters; needs domain-owned cleanup capabilities"},
	{From: "accountteardown", To: "domains/ec2/awsapi/snapshot", Debt: "accountteardown (dependency)", Reason: "EC2 reapers call the awsapi action adapters; needs domain-owned cleanup capabilities"},
	{From: "accountteardown", To: "domains/ec2/awsapi/volume", Debt: "accountteardown (dependency)", Reason: "EC2 reapers call the awsapi action adapters; needs domain-owned cleanup capabilities"},
	{From: "accountteardown", To: "domains/ec2/awsapi/vpc", Debt: "accountteardown (dependency)", Reason: "EC2 reapers call the awsapi action adapters; needs domain-owned cleanup capabilities"},

	{From: "agents/ecs/agent", To: "handlers/ecs", Debt: "agents/ecs/agent", Reason: "assignment/state wire types from the ECS implementation; needs a guest/controller contract"},
	{From: "agents/ecs/agent", To: "handlers/ecs/bus", Debt: "agents/ecs/agent", Reason: "assignment/state wire types from the ECS implementation; needs a guest/controller contract"},

	{From: "agents/eks/tokenwebhook", To: "handlers/eks", Debt: "agents/eks/tokenwebhook", Reason: "WebhookTokenReviewResult from the EKS implementation; needs an EKS contract or projection"},

	{From: "agents/rds/agent", To: "handlers/rds", Debt: "agents/rds/agent (dependency, narrowed)", Reason: "command protocol, wire and record types; needs a guest/controller wire contract"},

	{From: "cmd/aws-model-coverage", To: "domains/acm/awsapi", Debt: "cmd/aws-model-coverage", Reason: "reads declared ECR/ACM inventories; named exception, must not construct a registration"},
	{From: "cmd/aws-model-coverage", To: "domains/ecr/awsapi", Debt: "cmd/aws-model-coverage", Reason: "reads declared ECR/ACM inventories; named exception, must not construct a registration"},

	{From: "daemon", To: "domains/ec2/awsapi/instance", Debt: "Instance adapter consumers outside EC2", Reason: "daemon/eks_cp_control.go and rds_deps.go call the instance adapter; needs an EC2 capability"},

	{From: "domains/acm", To: "handlers/iam", Debt: "ACM: the IAM AES-GCM helper import (ACM relocation residual)", Reason: "IAM AES-GCM helpers; needs a shared crypto boundary"},

	{From: "domains/admission/quota", To: "domains/ec2/awsapi/eip", Debt: "domains/admission/quota (dependency, moved from handlers/quota unchanged)", Reason: "describe adapter used as a counter; needs a reader capability"},
	{From: "domains/admission/quota", To: "domains/ec2/awsapi/instance", Debt: "domains/admission/quota (dependency, moved from handlers/quota unchanged)", Reason: "vcpu.go counts instances through the instance adapter; needs an EC2 reader capability"},
	{From: "domains/admission/quota", To: "domains/ec2/awsapi/volume", Debt: "domains/admission/quota (dependency, moved from handlers/quota unchanged)", Reason: "describe adapter used as a counter; needs a reader capability"},
	{From: "domains/admission/quota", To: "domains/ec2/awsapi/vpc", Debt: "domains/admission/quota (dependency, moved from handlers/quota unchanged)", Reason: "describe adapter used as a counter; needs a reader capability"},
	{From: "domains/admission/quota", To: "domains/ec2/instancetypes", Debt: "domains/admission/quota → domains/ec2/instancetypes", Reason: "DefaultVCPUs sizing; needs a consumer-owned capability or EC2 projection"},
	{From: "domains/admission/quota", To: "gateway/elbv2", Debt: "domains/admission/quota (dependency, moved from handlers/quota unchanged)", Reason: "live ELBv2 counter; needs a reader capability"},
	{From: "domains/admission/quota", To: "handlers/rds", Debt: "domains/admission/quota (dependency, moved from handlers/quota unchanged)", Reason: "live RDS counter; needs a reader capability"},
	{From: "domains/admission/quota", To: "runtime/compute/vm", Debt: "domains/admission/quota (dependency, moved from handlers/quota unchanged)", Reason: "instance-record vCPU sweep; needs a reader capability"},

	{From: "domains/dns", To: "domains/network/reconcile", Debt: "domains/dns", Reason: "takes the reconcile lock from network's implementation"},
	{From: "domains/dns", To: "runtime/compute/vm", Debt: "domains/dns", Reason: "domain-to-runtime import of VM state"},

	{From: "domains/ec2/awsapi/instance", To: "handlers/iam", Debt: "domains/ec2/awsapi/instance (dependency)", Reason: "IAM edge needs an IAM capability"},
	{From: "domains/ec2/awsapi/instance", To: "runtime/compute/cache", Debt: "domains/ec2/awsapi/instance (dependency)", Reason: "node liveness and VM state; needs an EC2-owned projection"},
	{From: "domains/ec2/awsapi/instance", To: "runtime/compute/vm", Debt: "domains/ec2/awsapi/instance (dependency)", Reason: "node liveness and VM state; needs an EC2-owned projection"},

	{From: "domains/ec2/awsapi/spotinstance", To: "domains/admission/quota", Debt: "domains/ec2/awsapi/spotinstance (dependency)", Reason: "RequestSpotInstances.go uses IAMService and quota.Service"},
	{From: "domains/ec2/awsapi/spotinstance", To: "handlers/iam", Debt: "domains/ec2/awsapi/spotinstance (dependency)", Reason: "RequestSpotInstances.go uses IAMService and quota.Service"},

	{From: "domains/ec2/eip", To: "domains/network/topology", Debt: "EC2 → network implementation", Reason: "EC2 reads network implementation; clears with the EC2-owned projection"},

	{From: "domains/ec2/guestmetadata", To: "domains/dns", Debt: "Guest metadata: the cross-domain and runtime imports above", Reason: "guest-metadata relocation residual; needs capabilities, projections or contracts"},
	{From: "domains/ec2/guestmetadata", To: "handlers/iam", Debt: "Guest metadata: the cross-domain and runtime imports above", Reason: "guest-metadata relocation residual; needs capabilities, projections or contracts"},
	{From: "domains/ec2/guestmetadata", To: "handlers/sts", Debt: "Guest metadata: the cross-domain and runtime imports above", Reason: "guest-metadata relocation residual; needs capabilities, projections or contracts"},
	{From: "domains/ec2/guestmetadata", To: "runtime/compute/vm", Debt: "Guest metadata: the cross-domain and runtime imports above", Reason: "guest-metadata relocation residual; needs capabilities, projections or contracts"},

	{From: "domains/ec2/image", To: "utils", Debt: "utils", Reason: "consumes a recorded utils residual file awaiting its owner"},

	{From: "domains/ec2/instance", To: "domains/dns", Debt: "domains/ec2/instance", Reason: "cross-domain implementation import of DNS"},
	{From: "domains/ec2/instance", To: "domains/network/topology", Debt: "EC2 → network implementation", Reason: "EC2 reads network implementation; clears with the EC2-owned projection"},
	{From: "domains/ec2/instance", To: "runtime/compute/gpu", Debt: "domains/ec2/instance", Reason: "VM/GPU runtime state read directly; needs a domain-owned projection"},
	{From: "domains/ec2/instance", To: "runtime/compute/vm", Debt: "domains/ec2/instance", Reason: "VM/GPU runtime state read directly; needs a domain-owned projection"},
	{From: "domains/ec2/instance", To: "utils", Debt: "utils", Reason: "consumes a recorded utils residual file awaiting its owner"},

	{From: "domains/ec2/volume", To: "runtime/compute/vm", Debt: "Domain → runtime", Reason: "VM runtime state read directly; needs a domain-owned projection"},
	{From: "domains/ec2/volume", To: "utils", Debt: "utils", Reason: "consumes a recorded utils residual file awaiting its owner"},

	{From: "domains/ec2/vpc", To: "domains/network/external", Debt: "EC2 → network implementation", Reason: "EC2 reads network implementation; clears with the EC2-owned projection"},
	{From: "domains/ec2/vpc", To: "domains/network/external/dhcp", Debt: "EC2 → network implementation", Reason: "EC2 reads network implementation; clears with the EC2-owned projection"},
	{From: "domains/ec2/vpc", To: "domains/network/identifiers", Debt: "EC2 → network implementation", Reason: "EC2 reads network implementation; clears with the EC2-owned projection"},
	{From: "domains/ec2/vpc", To: "domains/network/topology", Debt: "EC2 → network implementation", Reason: "EC2 reads network implementation; clears with the EC2-owned projection"},

	{From: "domains/ecr/auth", To: "handlers/iam", Debt: "Cross-domain", Reason: "IAM implementation import; needs an IAM capability"},

	{From: "domains/network/host", To: "runtime/compute/vm", Debt: "domains/network", Reason: "domain-to-runtime import"},
	{From: "domains/network/host", To: "runtime/host/command", Debt: "domains/network", Reason: "domain-to-runtime import"},

	{From: "domains/network/reconcile", To: "domains/ec2/eip", Debt: "domains/network", Reason: "reconcile reads EC2 private records; clears with the EC2-owned projection"},
	{From: "domains/network/reconcile", To: "domains/ec2/igw", Debt: "domains/network", Reason: "reconcile reads EC2 private records; clears with the EC2-owned projection"},
	{From: "domains/network/reconcile", To: "domains/ec2/natgw", Debt: "domains/network", Reason: "reconcile reads EC2 private records; clears with the EC2-owned projection"},
	{From: "domains/network/reconcile", To: "domains/ec2/routetable", Debt: "domains/network", Reason: "reconcile reads EC2 private records; clears with the EC2-owned projection"},
	{From: "domains/network/reconcile", To: "domains/ec2/vpc", Debt: "domains/network", Reason: "reconcile reads EC2 private records; clears with the EC2-owned projection"},
	{From: "domains/network/reconcile", To: "runtime/compute/vm", Debt: "domains/network", Reason: "domain-to-runtime import"},

	{From: "domains/ochre", To: "domains/ec2/systeminstance", Debt: "Cross-domain", Reason: "consumes another domain's implementation rather than a capability"},
	{From: "domains/ochre", To: "domains/network/systemvpc", Debt: "Cross-domain", Reason: "consumes another domain's implementation rather than a capability"},
	{From: "domains/ochre", To: "gateway/bedrock", Debt: "gateway/bedrock", Reason: "Ochre domain core still lives in gateway/bedrock; needs the recorded split"},
	{From: "domains/ochre", To: "runtime/compute/gpu", Debt: "Domain → runtime", Reason: "GPU runtime state read directly; needs a domain-owned projection"},

	{From: "domains/ochre/vector", To: "handlers/iam", Debt: "Cross-domain", Reason: "consumes another domain's implementation rather than a capability"},

	{From: "gateway", To: "domains/ec2/awsapi", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/account", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/capacityreservation", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/eigw", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/eip", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/idem", Debt: "gateway/ec2.go (registration; ec2_idempotency.go)", Reason: "dispatch-time idempotency wrapper; moves with the EC2 dispatch map"},
	{From: "gateway", To: "domains/ec2/awsapi/igw", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/image", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/instance", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/key", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/launchtemplate", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/natgw", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/placementgroup", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/routetable", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/snapshot", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/spotinstance", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/tags", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/volume", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/vpc", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},
	{From: "gateway", To: "domains/ec2/awsapi/zone", Debt: "gateway/ec2.go (registration)", Reason: "EC2 dispatch map stays in gateway until the ingress registration seam"},

	{From: "gateway/bedrock", To: "handlers/iam", Debt: "IAM implementation consumers in STS, Ochre and ELBv2", Reason: "secret encrypt/decrypt helpers; needs a shared crypto boundary"},

	{From: "gateway/rds", To: "domains/ec2/awsapi/instance", Debt: "Instance adapter consumers outside EC2", Reason: "enginecatalog.go calls the instance adapter; needs an EC2 capability"},

	{From: "gateway/sts", To: "handlers/iam", Debt: "IAM implementation consumers in STS, Ochre and ELBv2", Reason: "GetCallerIdentity takes IAMService; needs an IAM capability"},

	{From: "gateway/tagging", To: "domains/ec2/tags", Debt: "close-out: gateway/tagging", Reason: "cross-service GetResources; clears when domains/tagging aggregates through per-domain capabilities"},
	{From: "gateway/tagging", To: "handlers/elbv2", Debt: "close-out: gateway/tagging", Reason: "cross-service GetResources; clears when domains/tagging aggregates through per-domain capabilities"},

	{From: "handlers/ecs", To: "domains/ec2/ebs/metadata", Debt: "close-out: handlers/ecs (ADR-0004 blocked-dependency list; inventory-ecs boundary imports)", Reason: "legacy ECS calls another domain's implementation as an internal API"},
	{From: "handlers/ecs", To: "domains/ec2/eip", Debt: "close-out: handlers/ecs (ADR-0004 blocked-dependency list; inventory-ecs boundary imports)", Reason: "legacy ECS calls another domain's implementation as an internal API"},
	{From: "handlers/ecs", To: "domains/ec2/instancetypes", Debt: "close-out: handlers/ecs (ADR-0004 blocked-dependency list; inventory-ecs boundary imports)", Reason: "legacy ECS calls another domain's implementation as an internal API"},
	{From: "handlers/ecs", To: "handlers/elbv2", Debt: "close-out: handlers/ecs (ADR-0004 blocked-dependency list; inventory-ecs boundary imports)", Reason: "legacy ECS calls another domain's implementation as an internal API"},
	{From: "handlers/ecs", To: "handlers/iam", Debt: "close-out: handlers/ecs (ADR-0004 blocked-dependency list; inventory-ecs boundary imports)", Reason: "legacy ECS calls another domain's implementation as an internal API"},

	{From: "handlers/eks", To: "domains/dns", Debt: "close-out: handlers/eks (ADR-0004 blocked-dependency list; inventory-eks boundary imports)", Reason: "legacy EKS calls another domain's or runtime implementation directly"},
	{From: "handlers/eks", To: "domains/ec2/ebs/metadata", Debt: "close-out: handlers/eks (ADR-0004 blocked-dependency list; inventory-eks boundary imports)", Reason: "legacy EKS calls another domain's or runtime implementation directly"},
	{From: "handlers/eks", To: "domains/ec2/instancetypes", Debt: "close-out: handlers/eks (ADR-0004 blocked-dependency list; inventory-eks boundary imports)", Reason: "legacy EKS calls another domain's or runtime implementation directly"},
	{From: "handlers/eks", To: "domains/ec2/placementgroup", Debt: "close-out: handlers/eks (ADR-0004 blocked-dependency list; inventory-eks boundary imports)", Reason: "legacy EKS calls another domain's or runtime implementation directly"},
	{From: "handlers/eks", To: "domains/ec2/systeminstance", Debt: "close-out: handlers/eks (ADR-0004 blocked-dependency list; inventory-eks boundary imports)", Reason: "legacy EKS calls another domain's or runtime implementation directly"},
	{From: "handlers/eks", To: "domains/network/systemvpc", Debt: "close-out: handlers/eks (ADR-0004 blocked-dependency list; inventory-eks boundary imports)", Reason: "legacy EKS calls another domain's or runtime implementation directly"},
	{From: "handlers/eks", To: "handlers/iam", Debt: "close-out: handlers/eks (ADR-0004 blocked-dependency list; inventory-eks boundary imports)", Reason: "legacy EKS calls another domain's or runtime implementation directly"},
	{From: "handlers/eks", To: "runtime/compute/vm", Debt: "close-out: handlers/eks (ADR-0004 blocked-dependency list; inventory-eks boundary imports)", Reason: "legacy EKS calls another domain's or runtime implementation directly"},

	{From: "handlers/elbv2", To: "domains/acm", Debt: "handlers/elbv2 (cross-domain)", Reason: "legacy ELBv2 cross-domain import; clears when ELBv2 is extracted behind consumer-owned capabilities"},
	{From: "handlers/elbv2", To: "domains/dns", Debt: "handlers/elbv2 (cross-domain)", Reason: "legacy ELBv2 cross-domain import; clears when ELBv2 is extracted behind consumer-owned capabilities"},
	{From: "handlers/elbv2", To: "domains/ec2/systeminstance", Debt: "handlers/elbv2 (cross-domain)", Reason: "legacy ELBv2 cross-domain import; clears when ELBv2 is extracted behind consumer-owned capabilities"},
	{From: "handlers/elbv2", To: "domains/ec2/vpc", Debt: "handlers/elbv2 (cross-domain)", Reason: "legacy ELBv2 cross-domain import; clears when ELBv2 is extracted behind consumer-owned capabilities"},
	{From: "handlers/elbv2", To: "domains/network/topology", Debt: "handlers/elbv2 (cross-domain)", Reason: "legacy ELBv2 cross-domain import; clears when ELBv2 is extracted behind consumer-owned capabilities"},
	{From: "handlers/elbv2", To: "handlers/iam", Debt: "IAM implementation consumers in STS, Ochre and ELBv2", Reason: "system-instance role ensurer; needs an IAM capability"},
	{From: "handlers/elbv2", To: "lbagent", Debt: "handlers/elbv2/{agent_types,haproxy,health_checker,nginx}.go import lbagent", Reason: "health-report and cert/PID-path vocabulary; needs a guest/controller contract"},

	{From: "handlers/iam", To: "admin", Debt: "handlers/iam/service_impl.go:30 imports admin", Reason: "credential and account helpers; awaits an IAM-credentials owner"},

	{From: "handlers/rds", To: "domains/dns", Debt: "close-out: handlers/rds (ADR-0004 blocked-dependency list; inventory-rds boundary imports)", Reason: "legacy RDS calls another domain's or runtime implementation directly"},
	{From: "handlers/rds", To: "domains/ec2/instance", Debt: "close-out: handlers/rds (ADR-0004 blocked-dependency list; inventory-rds boundary imports)", Reason: "legacy RDS calls another domain's or runtime implementation directly"},
	{From: "handlers/rds", To: "domains/ec2/instancetypes", Debt: "close-out: handlers/rds (ADR-0004 blocked-dependency list; inventory-rds boundary imports)", Reason: "legacy RDS calls another domain's or runtime implementation directly"},
	{From: "handlers/rds", To: "domains/ec2/systeminstance", Debt: "close-out: handlers/rds (ADR-0004 blocked-dependency list; inventory-rds boundary imports)", Reason: "legacy RDS calls another domain's or runtime implementation directly"},
	{From: "handlers/rds", To: "domains/network/systemvpc", Debt: "close-out: handlers/rds (ADR-0004 blocked-dependency list; inventory-rds boundary imports)", Reason: "legacy RDS calls another domain's or runtime implementation directly"},
	{From: "handlers/rds", To: "handlers/iam", Debt: "close-out: handlers/rds (ADR-0004 blocked-dependency list; inventory-rds boundary imports)", Reason: "legacy RDS calls another domain's or runtime implementation directly"},
	{From: "handlers/rds", To: "runtime/compute/vm", Debt: "close-out: handlers/rds (ADR-0004 blocked-dependency list; inventory-rds boundary imports)", Reason: "legacy RDS calls another domain's or runtime implementation directly"},

	{From: "handlers/sts", To: "handlers/eks", Debt: "handlers/sts/oidc_jwks.go:14 imports handlers/eks (Q-22)", Reason: "needs an EKS-owned OIDC projection or identity-owned contract"},
	{From: "handlers/sts", To: "handlers/iam", Debt: "IAM implementation consumers in STS, Ochre and ELBv2", Reason: "STS reads IAM service, trust-policy and OIDC-provider state; needs an IAM capability"},

	{From: "operator/cli", To: "accountteardown", Debt: "operator/cli (dependency)", Reason: "command tree imports implementations; needs public capabilities"},
	{From: "operator/cli", To: "admin", Debt: "operator/cli (dependency)", Reason: "command tree imports implementations; needs public capabilities"},
	{From: "operator/cli", To: "daemon", Debt: "operator/cli (dependency)", Reason: "command tree imports implementations; needs public capabilities"},
	{From: "operator/cli", To: "gateway", Debt: "operator/cli (dependency)", Reason: "command tree imports implementations; needs public capabilities"},
	{From: "operator/cli", To: "gateway/bedrock", Debt: "operator/cli (dependency)", Reason: "command tree imports implementations; needs public capabilities"},
	{From: "operator/cli", To: "handlers/eks", Debt: "operator/cli (dependency)", Reason: "command tree imports implementations; needs public capabilities"},
	{From: "operator/cli", To: "handlers/iam", Debt: "operator/cli (dependency)", Reason: "command tree imports implementations; needs public capabilities"},
	{From: "operator/cli", To: "vpcd", Debt: "operator/cli (dependency)", Reason: "command tree imports implementations; needs public capabilities"},
}
