package cluster

import (
	"context"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"time"

	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/nats-io/nats.go"
)

const defaultTimeout = 30 * time.Second

// subjectWork classifies an eks request subject by the work its daemon handler
// does before replying (ADR-0003:S8). The budget does not change with the
// class: no budget bounds a cost that grows with the resource.
type subjectWork int

const (
	// requestScaled handlers answer from records or write a bounded number of
	// them, and hand anything longer to a background owner.
	requestScaled subjectWork = iota
	// resourceScaledGap handlers still hold the request for work that grows with
	// the resource. They are not conformant; each is listed so that it is not
	// mistaken for one that is, and none may be added to silently.
	resourceScaledGap
)

// eksSubjects is every eks request subject, classified by its handler's work.
var eksSubjects = map[string]subjectWork{
	"eks.AssociateAccessPolicy":        requestScaled,
	"eks.CreateAccessEntry":            requestScaled,
	"eks.CreateAddon":                  requestScaled,
	"eks.CreateNodegroup":              requestScaled, // workers launch in the background
	"eks.DeleteAccessEntry":            requestScaled,
	"eks.DeleteAddon":                  requestScaled,
	"eks.DeleteCluster":                requestScaled, // teardown converges in the background
	"eks.DescribeAccessEntry":          requestScaled,
	"eks.DescribeAddon":                requestScaled,
	"eks.DescribeAddonVersions":        requestScaled,
	"eks.DescribeCluster":              requestScaled,
	"eks.DescribeNodegroup":            requestScaled,
	"eks.DisassociateAccessPolicy":     requestScaled,
	"eks.GetRecoveryDirective":         requestScaled,
	"eks.ListAccessEntries":            requestScaled,
	"eks.ListAccessPolicies":           requestScaled,
	"eks.ListAddons":                   requestScaled,
	"eks.ListAssociatedAccessPolicies": requestScaled,
	"eks.ListClusters":                 requestScaled,
	"eks.ListManagedWorkers":           requestScaled,
	"eks.ListNodegroups":               requestScaled,
	"eks.ListStagedAddonManifests":     requestScaled,
	"eks.ListTagsForResource":          requestScaled,
	"eks.SetRecoveryDirective":         requestScaled,
	"eks.TagResource":                  requestScaled,
	"eks.UntagResource":                requestScaled,
	"eks.UpdateAccessEntry":            requestScaled,
	"eks.UpdateAddon":                  requestScaled,

	// Reclaiming a FAILED cluster of the same name tears its infra down inline.
	"eks.CreateCluster": resourceScaledGap,
	// One routed terminate per worker, awaited.
	"eks.DeleteNodegroup": resourceScaledGap,
	// One awaited launch or terminate per worker added or removed.
	"eks.UpdateNodegroupConfig": resourceScaledGap,
	// Launches a control-plane VM and fences the old one inline.
	"eks.RestoreSnapshot": resourceScaledGap,
}

// classBudgets is the budget per class. A gap keeps the ordinary budget on
// purpose: raising it would hide the defect rather than remove it.
var classBudgets = map[subjectWork]time.Duration{
	requestScaled:     defaultTimeout,
	resourceScaledGap: defaultTimeout,
}

// requestTimeout is the gateway's budget for an eks request subject.
// TestS8_EKSSubjectBudgets fails for any subject missing from eksSubjects, so
// a new one is classified deliberately rather than by inheriting a default.
func requestTimeout(subject string) time.Duration {
	return classBudgets[eksSubjects[subject]]
}

// NATSEKSService is the gateway-side adapter that forwards every EKSService
// method as a NATS request to the daemon's matching subscriber.
type NATSEKSService struct {
	natsConn *nats.Conn
}

var _ EKSService = (*NATSEKSService)(nil)

// NewNATSEKSService returns an EKSService that uses NATS request-response.
func NewNATSEKSService(conn *nats.Conn) EKSService {
	return &NATSEKSService{natsConn: conn}
}

// --- Cluster ---

func (s *NATSEKSService) CreateCluster(ctx context.Context, input *eks.CreateClusterInput, accountID, callerPrincipalARN string) (*eks.CreateClusterOutput, error) {
	return natsmsg.NATSRequest[eks.CreateClusterOutput](ctx, s.natsConn, "eks.CreateCluster", input, requestTimeout("eks.CreateCluster"), accountID,
		natsmsg.NATSHeader{Key: natsmsg.PrincipalARNHeader, Value: callerPrincipalARN})
}

func (s *NATSEKSService) DescribeCluster(ctx context.Context, input *eks.DescribeClusterInput, accountID string) (*eks.DescribeClusterOutput, error) {
	return natsmsg.NATSRequest[eks.DescribeClusterOutput](ctx, s.natsConn, "eks.DescribeCluster", input, requestTimeout("eks.DescribeCluster"), accountID)
}

func (s *NATSEKSService) ListClusters(ctx context.Context, input *eks.ListClustersInput, accountID string) (*eks.ListClustersOutput, error) {
	return natsmsg.NATSRequest[eks.ListClustersOutput](ctx, s.natsConn, "eks.ListClusters", input, requestTimeout("eks.ListClusters"), accountID)
}

func (s *NATSEKSService) DeleteCluster(ctx context.Context, input *eks.DeleteClusterInput, accountID string) (*eks.DeleteClusterOutput, error) {
	return natsmsg.NATSRequest[eks.DeleteClusterOutput](ctx, s.natsConn, "eks.DeleteCluster", input, requestTimeout("eks.DeleteCluster"), accountID)
}

// --- Nodegroup ---

func (s *NATSEKSService) CreateNodegroup(ctx context.Context, input *eks.CreateNodegroupInput, accountID string) (*eks.CreateNodegroupOutput, error) {
	return natsmsg.NATSRequest[eks.CreateNodegroupOutput](ctx, s.natsConn, "eks.CreateNodegroup", input, requestTimeout("eks.CreateNodegroup"), accountID)
}

func (s *NATSEKSService) DescribeNodegroup(ctx context.Context, input *eks.DescribeNodegroupInput, accountID string) (*eks.DescribeNodegroupOutput, error) {
	return natsmsg.NATSRequest[eks.DescribeNodegroupOutput](ctx, s.natsConn, "eks.DescribeNodegroup", input, requestTimeout("eks.DescribeNodegroup"), accountID)
}

func (s *NATSEKSService) ListNodegroups(ctx context.Context, input *eks.ListNodegroupsInput, accountID string) (*eks.ListNodegroupsOutput, error) {
	return natsmsg.NATSRequest[eks.ListNodegroupsOutput](ctx, s.natsConn, "eks.ListNodegroups", input, requestTimeout("eks.ListNodegroups"), accountID)
}

func (s *NATSEKSService) UpdateNodegroupConfig(ctx context.Context, input *eks.UpdateNodegroupConfigInput, accountID string) (*eks.UpdateNodegroupConfigOutput, error) {
	return natsmsg.NATSRequest[eks.UpdateNodegroupConfigOutput](ctx, s.natsConn, "eks.UpdateNodegroupConfig", input, requestTimeout("eks.UpdateNodegroupConfig"), accountID)
}

func (s *NATSEKSService) DeleteNodegroup(ctx context.Context, input *eks.DeleteNodegroupInput, accountID string) (*eks.DeleteNodegroupOutput, error) {
	return natsmsg.NATSRequest[eks.DeleteNodegroupOutput](ctx, s.natsConn, "eks.DeleteNodegroup", input, requestTimeout("eks.DeleteNodegroup"), accountID)
}

// --- AccessEntry + AccessPolicy ---

func (s *NATSEKSService) CreateAccessEntry(ctx context.Context, input *eks.CreateAccessEntryInput, accountID string) (*eks.CreateAccessEntryOutput, error) {
	return natsmsg.NATSRequest[eks.CreateAccessEntryOutput](ctx, s.natsConn, "eks.CreateAccessEntry", input, requestTimeout("eks.CreateAccessEntry"), accountID)
}

func (s *NATSEKSService) DescribeAccessEntry(ctx context.Context, input *eks.DescribeAccessEntryInput, accountID string) (*eks.DescribeAccessEntryOutput, error) {
	return natsmsg.NATSRequest[eks.DescribeAccessEntryOutput](ctx, s.natsConn, "eks.DescribeAccessEntry", input, requestTimeout("eks.DescribeAccessEntry"), accountID)
}

func (s *NATSEKSService) ListAccessEntries(ctx context.Context, input *eks.ListAccessEntriesInput, accountID string) (*eks.ListAccessEntriesOutput, error) {
	return natsmsg.NATSRequest[eks.ListAccessEntriesOutput](ctx, s.natsConn, "eks.ListAccessEntries", input, requestTimeout("eks.ListAccessEntries"), accountID)
}

func (s *NATSEKSService) UpdateAccessEntry(ctx context.Context, input *eks.UpdateAccessEntryInput, accountID string) (*eks.UpdateAccessEntryOutput, error) {
	return natsmsg.NATSRequest[eks.UpdateAccessEntryOutput](ctx, s.natsConn, "eks.UpdateAccessEntry", input, requestTimeout("eks.UpdateAccessEntry"), accountID)
}

func (s *NATSEKSService) DeleteAccessEntry(ctx context.Context, input *eks.DeleteAccessEntryInput, accountID string) (*eks.DeleteAccessEntryOutput, error) {
	return natsmsg.NATSRequest[eks.DeleteAccessEntryOutput](ctx, s.natsConn, "eks.DeleteAccessEntry", input, requestTimeout("eks.DeleteAccessEntry"), accountID)
}

func (s *NATSEKSService) AssociateAccessPolicy(ctx context.Context, input *eks.AssociateAccessPolicyInput, accountID string) (*eks.AssociateAccessPolicyOutput, error) {
	return natsmsg.NATSRequest[eks.AssociateAccessPolicyOutput](ctx, s.natsConn, "eks.AssociateAccessPolicy", input, requestTimeout("eks.AssociateAccessPolicy"), accountID)
}

func (s *NATSEKSService) DisassociateAccessPolicy(ctx context.Context, input *eks.DisassociateAccessPolicyInput, accountID string) (*eks.DisassociateAccessPolicyOutput, error) {
	return natsmsg.NATSRequest[eks.DisassociateAccessPolicyOutput](ctx, s.natsConn, "eks.DisassociateAccessPolicy", input, requestTimeout("eks.DisassociateAccessPolicy"), accountID)
}

func (s *NATSEKSService) ListAssociatedAccessPolicies(ctx context.Context, input *eks.ListAssociatedAccessPoliciesInput, accountID string) (*eks.ListAssociatedAccessPoliciesOutput, error) {
	return natsmsg.NATSRequest[eks.ListAssociatedAccessPoliciesOutput](ctx, s.natsConn, "eks.ListAssociatedAccessPolicies", input, requestTimeout("eks.ListAssociatedAccessPolicies"), accountID)
}

func (s *NATSEKSService) ListAccessPolicies(ctx context.Context, input *eks.ListAccessPoliciesInput, accountID string) (*eks.ListAccessPoliciesOutput, error) {
	return natsmsg.NATSRequest[eks.ListAccessPoliciesOutput](ctx, s.natsConn, "eks.ListAccessPolicies", input, requestTimeout("eks.ListAccessPolicies"), accountID)
}

// --- Addons ---

func (s *NATSEKSService) ListAddons(ctx context.Context, input *eks.ListAddonsInput, accountID string) (*eks.ListAddonsOutput, error) {
	return natsmsg.NATSRequest[eks.ListAddonsOutput](ctx, s.natsConn, "eks.ListAddons", input, requestTimeout("eks.ListAddons"), accountID)
}

func (s *NATSEKSService) DescribeAddonVersions(ctx context.Context, input *eks.DescribeAddonVersionsInput, accountID string) (*eks.DescribeAddonVersionsOutput, error) {
	return natsmsg.NATSRequest[eks.DescribeAddonVersionsOutput](ctx, s.natsConn, "eks.DescribeAddonVersions", input, requestTimeout("eks.DescribeAddonVersions"), accountID)
}

func (s *NATSEKSService) CreateAddon(ctx context.Context, input *eks.CreateAddonInput, accountID string) (*eks.CreateAddonOutput, error) {
	return natsmsg.NATSRequest[eks.CreateAddonOutput](ctx, s.natsConn, "eks.CreateAddon", input, requestTimeout("eks.CreateAddon"), accountID)
}

func (s *NATSEKSService) DeleteAddon(ctx context.Context, input *eks.DeleteAddonInput, accountID string) (*eks.DeleteAddonOutput, error) {
	return natsmsg.NATSRequest[eks.DeleteAddonOutput](ctx, s.natsConn, "eks.DeleteAddon", input, requestTimeout("eks.DeleteAddon"), accountID)
}

func (s *NATSEKSService) DescribeAddon(ctx context.Context, input *eks.DescribeAddonInput, accountID string) (*eks.DescribeAddonOutput, error) {
	return natsmsg.NATSRequest[eks.DescribeAddonOutput](ctx, s.natsConn, "eks.DescribeAddon", input, requestTimeout("eks.DescribeAddon"), accountID)
}

func (s *NATSEKSService) ListStagedAddonManifests(ctx context.Context, input *ListStagedAddonManifestsInput, accountID string) (*ListStagedAddonManifestsOutput, error) {
	return natsmsg.NATSRequest[ListStagedAddonManifestsOutput](ctx, s.natsConn, "eks.ListStagedAddonManifests", input, requestTimeout("eks.ListStagedAddonManifests"), accountID)
}

func (s *NATSEKSService) ListManagedWorkers(ctx context.Context, input *ListManagedWorkersInput, accountID string) (*ListManagedWorkersOutput, error) {
	return natsmsg.NATSRequest[ListManagedWorkersOutput](ctx, s.natsConn, "eks.ListManagedWorkers", input, requestTimeout("eks.ListManagedWorkers"), accountID)
}

func (s *NATSEKSService) UpdateAddon(ctx context.Context, input *eks.UpdateAddonInput, accountID string) (*eks.UpdateAddonOutput, error) {
	return natsmsg.NATSRequest[eks.UpdateAddonOutput](ctx, s.natsConn, "eks.UpdateAddon", input, requestTimeout("eks.UpdateAddon"), accountID)
}

func (s *NATSEKSService) GetRecoveryDirective(ctx context.Context, input *GetRecoveryDirectiveInput, accountID string) (*GetRecoveryDirectiveOutput, error) {
	return natsmsg.NATSRequest[GetRecoveryDirectiveOutput](ctx, s.natsConn, "eks.GetRecoveryDirective", input, requestTimeout("eks.GetRecoveryDirective"), accountID)
}

func (s *NATSEKSService) SetRecoveryDirective(ctx context.Context, input *SetRecoveryDirectiveInput, accountID string) (*SetRecoveryDirectiveOutput, error) {
	return natsmsg.NATSRequest[SetRecoveryDirectiveOutput](ctx, s.natsConn, "eks.SetRecoveryDirective", input, requestTimeout("eks.SetRecoveryDirective"), accountID)
}

func (s *NATSEKSService) RestoreSnapshot(ctx context.Context, input *RestoreSnapshotInput, accountID string) (*RestoreSnapshotOutput, error) {
	return natsmsg.NATSRequest[RestoreSnapshotOutput](ctx, s.natsConn, "eks.RestoreSnapshot", input, requestTimeout("eks.RestoreSnapshot"), accountID)
}

// --- Tags ---

func (s *NATSEKSService) TagResource(ctx context.Context, input *eks.TagResourceInput, accountID string) (*eks.TagResourceOutput, error) {
	return natsmsg.NATSRequest[eks.TagResourceOutput](ctx, s.natsConn, "eks.TagResource", input, requestTimeout("eks.TagResource"), accountID)
}

func (s *NATSEKSService) UntagResource(ctx context.Context, input *eks.UntagResourceInput, accountID string) (*eks.UntagResourceOutput, error) {
	return natsmsg.NATSRequest[eks.UntagResourceOutput](ctx, s.natsConn, "eks.UntagResource", input, requestTimeout("eks.UntagResource"), accountID)
}

func (s *NATSEKSService) ListTagsForResource(ctx context.Context, input *eks.ListTagsForResourceInput, accountID string) (*eks.ListTagsForResourceOutput, error) {
	return natsmsg.NATSRequest[eks.ListTagsForResourceOutput](ctx, s.natsConn, "eks.ListTagsForResource", input, requestTimeout("eks.ListTagsForResource"), accountID)
}
