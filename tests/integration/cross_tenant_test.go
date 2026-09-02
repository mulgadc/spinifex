//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	awscreds "github.com/aws/aws-sdk-go/aws/credentials"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_acm "github.com/mulgadc/spinifex/spinifex/gateway/acm"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	gateway_ecs "github.com/mulgadc/spinifex/spinifex/gateway/ecs"
	gateway_eks "github.com/mulgadc/spinifex/spinifex/gateway/eks"
	gateway_elbv2 "github.com/mulgadc/spinifex/spinifex/gateway/elbv2"
	gateway_iam "github.com/mulgadc/spinifex/spinifex/gateway/iam"
	"github.com/mulgadc/spinifex/spinifex/gateway/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tenant boundary this suite pins: a caller in one account must never have
// a resource ARN belonging to another account reach the policy evaluator
// unchanged. Two enforcement shapes satisfy that, and both are represented
// below, because asserting one spelling everywhere would fail services that
// legitimately use the other.
type boundaryShape uint8

const (
	// The gate rewrites the account (and region) segment of a caller-supplied
	// ARN onto the caller's own before evaluating. A Deny fencing the caller's
	// own resource therefore still fires when the request spells the same
	// resource with a foreign account, which is the observable proof.
	boundaryReanchored boundaryShape = iota
	// The gate refuses a caller-supplied ARN naming another account outright,
	// ahead of evaluation, as a request-validation fault.
	boundaryRejected
)

// probeUser is the single IAM user every case signs as. Its inline policy is
// rewritten between runs rather than a new user minted per run: the gateway
// resolves policies on every request, so one principal covers every policy
// shape a case needs without the churn.
const (
	probeUser       = "probe"
	probePolicyName = "probe-inline"
)

// crossTenantCase is one gateway action that takes a caller-supplied resource
// ARN, in the two spellings the boundary has to keep apart.
type crossTenantCase struct {
	// service is the SigV4 signing name, which is also what the gateway reads
	// the service from — so it selects both the signature and the route.
	service string
	action  string
	// arnFor builds the ARN this action names, anchored on the given account.
	arnFor func(accountID string) string
	// request builds the signed HTTP request naming resourceARN, signed for
	// the case's service.
	request requestBuilder
	shape   boundaryShape
}

// requestBuilder builds one signed probe. service is passed rather than closed
// over so the case's single service field drives both the signature and the
// route, and cannot drift from the one the subtest name reports.
type requestBuilder func(t *testing.T, gw *Gateway, creds *awscreds.Credentials, service, resourceARN string) *http.Request

// iamAction is the policy action the case's statements name, formatted by the
// same function the gate formats it with, so a namespace that diverges from the
// wire service name is a failure here rather than a silent mismatch.
func (c crossTenantCase) iamAction() string {
	return policy.IAMAction(c.service, c.action)
}

// foreignCode is the error the case's foreign-ARN runs must produce.
func (c crossTenantCase) foreignCode() string {
	if c.shape == boundaryRejected {
		return awserrors.ErrorInvalidParameterValue
	}
	return awserrors.ErrorAccessDenied
}

func acmCertificateARN(accountID string) string {
	return "arn:aws:acm:" + testRegion + ":" + accountID + ":certificate/aaaabbbb-1111-2222-3333-cccccccccccc"
}

func ecsClusterARN(accountID string) string {
	return "arn:aws:ecs:" + testRegion + ":" + accountID + ":cluster/prod"
}

func ecrRepositoryARN(accountID string) string {
	return "arn:aws:ecr:" + testRegion + ":" + accountID + ":repository/app"
}

func eksClusterARN(accountID string) string {
	return "arn:aws:eks:" + testRegion + ":" + accountID + ":cluster/prod"
}

func elbv2LoadBalancerARN(accountID string) string {
	return "arn:aws:elasticloadbalancing:" + testRegion + ":" + accountID +
		":loadbalancer/app/prod/0123456789abcdef"
}

// crossTenantPolicyARN names one managed policy, anchored on the given account.
func crossTenantPolicyARN(accountID string) string {
	return iamPolicyARN(accountID, "Admin")
}

func rdsInstanceARN(accountID string) string {
	return "arn:aws:rds:" + testRegion + ":" + accountID + ":db:prod"
}

// crossTenantCases covers every gateway service whose scope table resolves a
// resource from an ARN the caller supplies. A service is absent only when
// naming another account's resource is not expressible at all — see
// TestCrossTenant_EveryScopedActionIsAccountedFor, which holds that reasoning
// against the live scope tables.
func crossTenantCases() []crossTenantCase {
	return []crossTenantCase{
		{
			service: "acm", action: "DescribeCertificate",
			arnFor: acmCertificateARN, shape: boundaryReanchored,
			request: jsonTarget("CertificateManager.DescribeCertificate", func(resourceARN string) string {
				return `{"CertificateArn":"` + resourceARN + `"}`
			}),
		},
		{
			service: "acm", action: "DeleteCertificate",
			arnFor: acmCertificateARN, shape: boundaryReanchored,
			request: jsonTarget("CertificateManager.DeleteCertificate", func(resourceARN string) string {
				return `{"CertificateArn":"` + resourceARN + `"}`
			}),
		},
		{
			service: "ecs", action: "ListTagsForResource",
			arnFor: ecsClusterARN, shape: boundaryReanchored,
			request: jsonTarget("AmazonEC2ContainerServiceV20141113.ListTagsForResource", func(resourceARN string) string {
				return `{"resourceArn":"` + resourceARN + `"}`
			}),
		},
		{
			service: "ecs", action: "DescribeClusters",
			arnFor: ecsClusterARN, shape: boundaryReanchored,
			request: jsonTarget("AmazonEC2ContainerServiceV20141113.DescribeClusters", func(resourceARN string) string {
				return `{"clusters":["` + resourceARN + `"]}`
			}),
		},
		{
			service: "ecr", action: "ListTagsForResource",
			arnFor: ecrRepositoryARN, shape: boundaryReanchored,
			request: jsonTarget(gateway_ecrapi.TargetPrefix+".ListTagsForResource", func(resourceARN string) string {
				return `{"resourceArn":"` + resourceARN + `"}`
			}),
		},
		{
			service: "eks", action: "ListTagsForResource",
			arnFor: eksClusterARN, shape: boundaryReanchored,
			request: func(t *testing.T, gw *Gateway, creds *awscreds.Credentials, service, resourceARN string) *http.Request {
				t.Helper()
				return signedRequest(t, gw, creds, service, http.MethodGet,
					"/tags/"+url.PathEscape(resourceARN), nil, nil)
			},
		},
		{
			service: "elasticloadbalancing", action: "DeleteLoadBalancer",
			arnFor: elbv2LoadBalancerARN, shape: boundaryRejected,
			request: queryProtocolCase("DeleteLoadBalancer", "2015-12-01", func(resourceARN string) map[string]string {
				return map[string]string{"LoadBalancerArn": resourceARN}
			}),
		},
		{
			service: "elasticloadbalancing", action: "AddTags",
			arnFor: elbv2LoadBalancerARN, shape: boundaryRejected,
			request: queryProtocolCase("AddTags", "2015-12-01", func(resourceARN string) map[string]string {
				return map[string]string{
					"ResourceArns.member.1": resourceARN,
					"Tags.member.1.Key":     "env",
					"Tags.member.1.Value":   "prod",
				}
			}),
		},
		{
			service: "rds", action: "ListTagsForResource",
			arnFor: rdsInstanceARN, shape: boundaryRejected,
			request: queryProtocolCase("ListTagsForResource", "2014-10-31", func(resourceARN string) map[string]string {
				return map[string]string{"ResourceName": resourceARN}
			}),
		},
		{
			service: "iam", action: "GetPolicy",
			arnFor: crossTenantPolicyARN, shape: boundaryReanchored,
			request: queryProtocolCase("GetPolicy", "2010-05-08", func(resourceARN string) map[string]string {
				return map[string]string{"PolicyArn": resourceARN}
			}),
		},
	}
}

// TestCrossTenant_ForeignResourceARNNeverAuthorizes is the suite proper. For
// every case it runs four probes against two real, independently-owned
// accounts, all as the same caller in the first account:
//
//	own/allowed  — names its own resource under a wildcard grant. The control:
//	               without it a foreign probe could pass because the route,
//	               body or action was wrong rather than because the boundary
//	               held.
//	own/fenced   — names its own resource with that resource explicitly
//	               denied. Proves the fence used by the next probe is real.
//	foreign/fenced — names the OTHER account's resource, same fence. The fence
//	               names the caller's own ARN, so it can only fire if the gate
//	               re-anchored the foreign ARN onto the caller. A permit here
//	               is a fence the caller escapes by rewriting an account id.
//	foreign/granted — names the other account's resource holding a grant on
//	               exactly that foreign ARN and nothing else. A permit here is
//	               a grant on another tenant's resource taking effect.
func TestCrossTenant_ForeignResourceARNNeverAuthorizes(t *testing.T) {
	gw := StartGateway(t)

	home, creds := createProbePrincipal(t, gw, "cross-tenant-home")
	foreign, err := gw.Config.IAMService.CreateAccount("cross-tenant-foreign")
	require.NoError(t, err, "create foreign account")
	require.NotEqual(t, home, foreign.AccountID, "the two tenants share an account id")

	for _, tc := range crossTenantCases() {
		t.Run(tc.service+"/"+tc.action, func(t *testing.T) {
			ownARN := tc.arnFor(home)
			foreignARN := tc.arnFor(foreign.AccountID)
			require.NotEqual(t, ownARN, foreignARN, "the two ARNs are indistinguishable")

			wide := policyDocument(statementJSON("Allow", tc.iamAction(), "arn:aws:*:*:*:*"))
			fenced := policyDocument(
				statementJSON("Allow", tc.iamAction(), "arn:aws:*:*:*:*"),
				statementJSON("Deny", tc.iamAction(), ownARN),
			)
			foreignGrant := policyDocument(statementJSON("Allow", tc.iamAction(), foreignARN))

			putProbePolicy(t, gw, home, wide)
			assert.NotEqual(t, awserrors.ErrorAccessDenied, sendCase(t, gw, tc, creds, ownARN),
				"own resource under a wildcard grant was denied — the case never reaches the boundary")

			putProbePolicy(t, gw, home, fenced)
			assert.Equal(t, awserrors.ErrorAccessDenied, sendCase(t, gw, tc, creds, ownARN),
				"the fence on the caller's own resource did not fire")

			assert.Equal(t, tc.foreignCode(), sendCase(t, gw, tc, creds, foreignARN),
				"the fence was escaped by respelling the resource with another account's id")

			putProbePolicy(t, gw, home, foreignGrant)
			assert.Equal(t, tc.foreignCode(), sendCase(t, gw, tc, creds, foreignARN),
				"a grant naming another account's resource authorized the request")
		})
	}
}

// TestCrossTenant_EveryScopedActionIsAccountedFor keeps the case table honest
// against the scope tables it draws from. Every scoped action is either probed
// by a case above or carries a written reason for not needing its own probe. A
// new scoped action is in neither set and fails here, so it cannot join with a
// silent account-wide grant and no cross-tenant coverage.
func TestCrossTenant_EveryScopedActionIsAccountedFor(t *testing.T) {
	cased := map[string]map[string]bool{}
	for _, tc := range crossTenantCases() {
		if cased[tc.service] == nil {
			cased[tc.service] = map[string]bool{}
		}
		cased[tc.service][tc.action] = true
	}

	for _, svc := range scopedServices() {
		t.Run(svc.name, func(t *testing.T) {
			for _, action := range svc.actions {
				_, excused := svc.uncased[action]
				if cased[svc.name][action] == excused {
					t.Errorf("%s:%s is %s — every scoped action needs exactly one of a "+
						"cross-tenant case or an entry in uncased saying why it does not need one",
						svc.name, action, bothOrNeither(cased[svc.name][action]))
				}
			}
			for action := range svc.uncased {
				assert.Contains(t, svc.actions, action,
					"uncased names %s:%s, which the scope table no longer has", svc.name, action)
			}
		})
	}
}

func bothOrNeither(cased bool) string {
	if cased {
		return "both cased and excused"
	}
	return "neither cased nor excused"
}

// scopedService pairs a service's scope table with the actions that need no
// cross-tenant case of their own, each with the reason.
type scopedService struct {
	name    string
	actions []string
	uncased map[string]string
}

// The reasons an action needs no cross-tenant probe of its own. byName and
// accountLevel mean no request can name another tenant's resource at all;
// sameHelper means the action reaches the boundary through the very resolver a
// cased action already drives, so a second probe would exercise the same line.
const (
	byName       = "named by name or id, which carries no account segment"
	accountLevel = "account-level: the action names no resource"
)

func sameHelper(casedAction string) string {
	return "resolved by the helper " + casedAction + "'s case drives"
}

func scopedServices() []scopedService {
	return []scopedService{
		{name: "acm", actions: gateway_acm.ScopedActions(), uncased: map[string]string{
			"GetCertificate":            sameHelper("DescribeCertificate"),
			"ListTagsForCertificate":    sameHelper("DescribeCertificate"),
			"AddTagsToCertificate":      sameHelper("DescribeCertificate"),
			"RemoveTagsFromCertificate": sameHelper("DescribeCertificate"),
			"ImportCertificate":         sameHelper("DescribeCertificate"),
			"RequestCertificate":        "the body's CertificateArn is discarded: the request creates a certificate",
			"ListCertificates":          accountLevel,
		}},

		// Every ECS identifier is folded through a ShortName/ShortID helper and
		// rebuilt on the caller's account, so a full ARN is accepted wherever a
		// name is. The two cases drive the tag and the cluster-list resolvers.
		{name: "ecs", actions: gateway_ecs.ScopedActions(), uncased: map[string]string{
			"TagResource":                   sameHelper("ListTagsForResource"),
			"UntagResource":                 sameHelper("ListTagsForResource"),
			"CreateCluster":                 sameHelper("DescribeClusters"),
			"DeleteCluster":                 sameHelper("DescribeClusters"),
			"UpdateCluster":                 sameHelper("DescribeClusters"),
			"PutClusterCapacityProviders":   sameHelper("DescribeClusters"),
			"ProvisionCapacity":             sameHelper("DescribeClusters"),
			"CreateService":                 sameHelper("DescribeClusters"),
			"UpdateService":                 sameHelper("DescribeClusters"),
			"DeleteService":                 sameHelper("DescribeClusters"),
			"DescribeServices":              sameHelper("DescribeClusters"),
			"StopTask":                      sameHelper("DescribeClusters"),
			"DescribeTasks":                 sameHelper("DescribeClusters"),
			"ReportTaskGPU":                 sameHelper("DescribeClusters"),
			"SubmitTaskStateChange":         sameHelper("DescribeClusters"),
			"DeregisterContainerInstance":   sameHelper("DescribeClusters"),
			"DescribeContainerInstances":    sameHelper("DescribeClusters"),
			"UpdateContainerInstancesState": sameHelper("DescribeClusters"),
			"PollAssignments":               sameHelper("DescribeClusters"),
			"DeregisterTaskDefinition":      sameHelper("DescribeClusters"),
			"RunTask":                       sameHelper("DescribeClusters"),
			"StartTask":                     sameHelper("DescribeClusters"),
			"CreateCapacityProvider":        sameHelper("DescribeClusters"),
			"DeleteCapacityProvider":        sameHelper("DescribeClusters"),
			"DescribeCapacityProviders":     sameHelper("DescribeClusters"),
			"ListClusters":                  accountLevel,
			"ListServices":                  accountLevel,
			"ListServicesByNamespace":       accountLevel,
			"ListTasks":                     accountLevel,
			"RegisterContainerInstance":     accountLevel,
			"ListContainerInstances":        accountLevel,
			"RegisterTaskDefinition":        accountLevel,
			"DescribeTaskDefinition":        accountLevel,
			"ListTaskDefinitions":           accountLevel,
			"ListTaskDefinitionFamilies":    accountLevel,
			"PutAccountSetting":             accountLevel,
			"ListAccountSettings":           accountLevel,
		}},

		// Only the three tag actions read an ARN; every other ECR action names
		// its repository by bare name.
		{name: "ecr", actions: gateway_ecrapi.ScopedActions(), uncased: ecrUncased()},

		// Same shape as ECR: the tag actions read an ARN, the rest name a
		// cluster, nodegroup or addon by path segment.
		{name: "eks", actions: gateway_eks.ScopedActions(), uncased: eksUncased()},

		{name: "elasticloadbalancing", actions: gateway_elbv2.ScopedActions(), uncased: elbv2Uncased()},

		{name: "iam", actions: gateway_iam.ScopedActions(), uncased: iamUncased()},
	}
}

func ecrUncased() map[string]string {
	uncased := map[string]string{
		"TagResource":   sameHelper("ListTagsForResource"),
		"UntagResource": sameHelper("ListTagsForResource"),
	}
	for _, action := range []string{
		"GetAuthorizationToken", "GetRegistryPolicy", "PutRegistryPolicy", "DescribeRegistry",
		"GetRegistryScanningConfiguration", "PutRegistryScanningConfiguration",
		"BatchGetRepositoryScanningConfiguration", "PutReplicationConfiguration", "ListRepositories",
	} {
		uncased[action] = accountLevel
	}
	for _, action := range []string{
		"CreateRepository", "DeleteRepository", "DescribeRepositories", "PutImageTagMutability",
		"BatchGetImage", "BatchCheckLayerAvailability", "BatchDeleteImage", "PutImage", "ListImages",
		"DescribeImages", "GetDownloadUrlForLayer", "InitiateLayerUpload", "UploadLayerPart",
		"CompleteLayerUpload", "ReplicateImage", "SetRepositoryPolicy", "GetRepositoryPolicy",
		"DeleteRepositoryPolicy", "PutImageScanningConfiguration", "GetImageScanningConfiguration",
		"StartImageScan", "DescribeImageScanFindings", "PutLifecyclePolicy", "GetLifecyclePolicy",
		"DeleteLifecyclePolicy", "StartLifecyclePolicyPreview", "GetLifecyclePolicyPreview",
	} {
		uncased[action] = byName
	}
	return uncased
}

func eksUncased() map[string]string {
	uncased := map[string]string{
		"TagResource":   sameHelper("ListTagsForResource"),
		"UntagResource": sameHelper("ListTagsForResource"),
	}
	for _, action := range []string{
		"AssociateAccessPolicy", "AssociateIdentityProviderConfig", "CreateAccessEntry", "CreateAddon",
		"CreateCluster", "CreateNodegroup", "DeleteAccessEntry", "DeleteAddon", "DeleteCluster",
		"DeleteNodegroup", "DescribeAccessEntry", "DescribeAddon", "DescribeAddonVersions",
		"DescribeCluster", "DescribeIdentityProviderConfig", "DescribeNodegroup",
		"DisassociateAccessPolicy", "DisassociateIdentityProviderConfig", "GetRecoveryDirective",
		"ListAccessEntries", "ListAccessPolicies", "ListAddons", "ListAssociatedAccessPolicies",
		"ListClusters", "ListIdentityProviderConfigs", "ListInternalAddons", "ListNodegroups",
		"PublishInternal", "UpdateAccessEntry", "UpdateAddon", "UpdateClusterConfig",
		"UpdateClusterVersion", "UpdateNodegroupConfig", "UpdateNodegroupVersion", "WebhookTokenReview",
	} {
		uncased[action] = byName
	}
	return uncased
}

func elbv2Uncased() map[string]string {
	uncased := map[string]string{}
	// Every caller-supplied ELBv2 ARN goes through the one checkARN guard that
	// DeleteLoadBalancer's case drives, whichever field names it.
	for _, action := range []string{
		"ModifyLoadBalancerAttributes", "DescribeLoadBalancerAttributes", "SetSecurityGroups",
		"SetSubnets", "SetIpAddressType", "DeleteTargetGroup", "ModifyTargetGroup",
		"ModifyTargetGroupAttributes", "DescribeTargetGroupAttributes", "RegisterTargets",
		"DeregisterTargets", "DescribeTargetHealth", "CreateListener", "ModifyListener",
		"DeleteListener", "AddListenerCertificates", "RemoveListenerCertificates",
		"DescribeListenerCertificates", "DescribeListenerAttributes", "ModifyListenerAttributes",
		"CreateRule", "ModifyRule", "DeleteRule", "SetRulePriorities",
	} {
		uncased[action] = sameHelper("DeleteLoadBalancer")
	}
	for _, action := range []string{"RemoveTags", "DescribeTags"} {
		uncased[action] = sameHelper("AddTags")
	}
	for _, action := range []string{"CreateLoadBalancer", "CreateTargetGroup"} {
		uncased[action] = byName
	}
	for _, action := range []string{
		"DescribeLoadBalancers", "DescribeTargetGroups", "DescribeListeners", "DescribeRules",
		"DescribeSSLPolicies", "LBAgentHeartbeat", "GetLBConfig",
	} {
		uncased[action] = accountLevel
	}
	return uncased
}

func iamUncased() map[string]string {
	uncased := map[string]string{}
	// The six siblings of GetPolicy read PolicyArn through the same branch.
	for _, action := range []string{
		"GetPolicyVersion", "ListPolicyVersions", "DeletePolicy", "TagPolicy", "UntagPolicy",
		"ListPolicyTags",
	} {
		uncased[action] = sameHelper("GetPolicy")
	}
	// The OIDC provider ARN is the one other caller-supplied ARN in the table.
	// It is reduced to its host and path and rebuilt on the caller's account,
	// so the account segment the caller wrote never reaches the evaluator.
	for _, action := range []string{
		"GetOpenIDConnectProvider", "DeleteOpenIDConnectProvider", "TagOpenIDConnectProvider",
		"UntagOpenIDConnectProvider", "ListOpenIDConnectProviderTags",
	} {
		uncased[action] = "the provider ARN is reduced to its host and path and rebuilt on the caller's account"
	}
	for _, action := range []string{
		"ListUsers", "ListPolicies", "ListRoles", "ListInstanceProfiles",
		"ListOpenIDConnectProviders", "ListGroups", "GetAccountSummary",
	} {
		uncased[action] = accountLevel
	}
	// Everything else names a user, role, group, instance profile or access key
	// by bare name, which carries no account segment. Listed rather than
	// defaulted, so a new IAM action fails the accounting instead of inheriting
	// an excuse.
	for _, action := range []string{
		"AddRoleToInstanceProfile", "AddUserToGroup", "AttachGroupPolicy", "AttachRolePolicy",
		"AttachUserPolicy", "CreateAccessKey", "CreateGroup", "CreateInstanceProfile",
		"CreateOpenIDConnectProvider", "CreatePolicy", "CreateRole", "CreateUser",
		"DeleteAccessKey", "DeleteGroup", "DeleteGroupPolicy", "DeleteInstanceProfile",
		"DeleteRole", "DeleteRolePolicy", "DeleteUser", "DeleteUserPolicy",
		"DeleteOpenIDConnectProvider", "DetachGroupPolicy", "DetachRolePolicy", "DetachUserPolicy",
		"GetGroup", "GetGroupPolicy", "GetInstanceProfile", "GetRole", "GetRolePolicy",
		"GetUser", "GetUserPolicy", "ListAccessKeys", "ListAttachedGroupPolicies",
		"ListAttachedRolePolicies", "ListAttachedUserPolicies", "ListGroupPolicies",
		"ListGroupsForUser", "ListInstanceProfileTags", "ListInstanceProfilesForRole",
		"ListRolePolicies", "ListRoleTags", "ListUserPolicies", "ListUserTags",
		"PutGroupPolicy", "PutRolePolicy", "PutUserPolicy", "RemoveRoleFromInstanceProfile",
		"RemoveUserFromGroup", "TagInstanceProfile", "TagRole", "TagUser",
		"UntagInstanceProfile", "UntagRole", "UntagUser", "UpdateAccessKey",
		"UpdateAssumeRolePolicy", "UpdateRole",
	} {
		uncased[action] = byName
	}
	return uncased
}

// sendCase issues one probe and returns the AWS error code, or "" for a 2xx.
func sendCase(t *testing.T, gw *Gateway, tc crossTenantCase, creds *awscreds.Credentials, resourceARN string) string {
	t.Helper()
	req := tc.request(t, gw, creds, tc.service, resourceARN)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "%s:%s", tc.service, tc.action)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ""
	}
	return awsErrorCode(body)
}

// awsErrorCode reads the error code out of either envelope the gateway emits:
// AWS JSON 1.1's {"__type":"<Code>Exception"} or the XML <Code> element. The
// "Exception" suffix is dropped so both spell one code.
func awsErrorCode(body []byte) string {
	var jsonErr struct {
		Type string `json:"__type"`
	}
	if err := json.Unmarshal(body, &jsonErr); err == nil && jsonErr.Type != "" {
		return strings.TrimSuffix(jsonErr.Type, "Exception")
	}

	var xmlErr struct {
		Code   string `xml:"Error>Code"`
		Nested string `xml:"Errors>Error>Code"`
	}
	if err := xml.Unmarshal(body, &xmlErr); err == nil {
		if xmlErr.Code != "" {
			return xmlErr.Code
		}
		if xmlErr.Nested != "" {
			return xmlErr.Nested
		}
	}
	return fmt.Sprintf("unparseable error envelope: %s", body)
}

// jsonTarget builds an AWS JSON 1.1 request factory: POST / with the action in
// X-Amz-Target, which is how the gateway routes ACM, ECS and the ECR control
// plane.
func jsonTarget(target string, body func(resourceARN string) string) requestBuilder {
	return func(t *testing.T, gw *Gateway, creds *awscreds.Credentials, service, resourceARN string) *http.Request {
		t.Helper()
		return signedRequest(t, gw, creds, service, http.MethodPost, "/",
			[]byte(body(resourceARN)), map[string]string{
				"Content-Type": "application/x-amz-json-1.1",
				"X-Amz-Target": target,
			})
	}
}

// queryAction builds a query-protocol request factory: a form-encoded POST /
// carrying Action, Version and the caller's params, which is how the gateway
// routes ELBv2, RDS and IAM.
func queryProtocolCase(action, version string, params func(resourceARN string) map[string]string) requestBuilder {
	return func(t *testing.T, gw *Gateway, creds *awscreds.Credentials, service, resourceARN string) *http.Request {
		t.Helper()
		form := url.Values{}
		form.Set("Action", action)
		form.Set("Version", version)
		for k, v := range params(resourceARN) {
			form.Set(k, v)
		}
		return signedRequest(t, gw, creds, service, http.MethodPost, "/",
			[]byte(form.Encode()), map[string]string{
				"Content-Type": "application/x-www-form-urlencoded; charset=utf-8",
			})
	}
}

// signedRequest builds and SigV4-signs a request against the harness gateway.
// Signing is mandatory even for probes expected to be denied: the gateway
// rejects on authentication long before it reads an action.
func signedRequest(t *testing.T, gw *Gateway, creds *awscreds.Credentials, service, method, path string, body []byte, headers map[string]string) *http.Request {
	t.Helper()

	req, err := http.NewRequest(method, gw.Server.URL+path, bytes.NewReader(body))
	require.NoError(t, err, "build %s %s", method, path)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	signer := v4.NewSigner(creds)
	_, err = signer.Sign(req, bytes.NewReader(body), service, testRegion, time.Now())
	require.NoError(t, err, "sign %s %s", service, path)
	return req
}

// createProbePrincipal mints a tenant account holding probeUser with an access
// key, and returns the account id plus credentials signed as that user. No
// policy is attached: every case installs the one it needs via putProbePolicy.
func createProbePrincipal(t *testing.T, gw *Gateway, name string) (string, *awscreds.Credentials) {
	t.Helper()

	acct, err := gw.Config.IAMService.CreateAccount(name)
	require.NoError(t, err, "create account %s", name)

	_, err = gw.Config.IAMService.CreateUser(acct.AccountID, &iam.CreateUserInput{
		UserName: aws.String(probeUser),
	})
	require.NoError(t, err, "create %s in %s", probeUser, name)

	key, err := gw.Config.IAMService.CreateAccessKey(acct.AccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String(probeUser),
	})
	require.NoError(t, err, "create access key in %s", name)

	return acct.AccountID, awscreds.NewStaticCredentials(
		aws.StringValue(key.AccessKey.AccessKeyId),
		aws.StringValue(key.AccessKey.SecretAccessKey),
		"")
}

// putProbePolicy replaces probeUser's inline policy. Policies are resolved on
// every request, so the next probe sees this document with no credential
// refresh.
func putProbePolicy(t *testing.T, gw *Gateway, accountID, document string) {
	t.Helper()
	_, err := gw.Config.IAMService.PutUserPolicy(accountID, &iam.PutUserPolicyInput{
		UserName:       aws.String(probeUser),
		PolicyName:     aws.String(probePolicyName),
		PolicyDocument: aws.String(document),
	})
	require.NoError(t, err, "put probe policy")
}

func policyDocument(statements ...string) string {
	return `{"Version":"2012-10-17","Statement":[` + strings.Join(statements, ",") + `]}`
}

func statementJSON(effect, action, resource string) string {
	return `{"Effect":"` + effect + `","Action":"` + action + `","Resource":"` + resource + `"}`
}
