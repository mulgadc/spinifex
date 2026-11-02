package awsgw

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
)

// codeStoreRoleIAM is the IAM surface the code-store role adapter uses.
type codeStoreRoleIAM interface {
	CreateRole(accountID string, input *iam.CreateRoleInput) (*iam.CreateRoleOutput, error)
	GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error)
	ListRoleTags(accountID string, input *iam.ListRoleTagsInput) (*iam.ListRoleTagsOutput, error)
	ListRolePolicies(accountID string, input *iam.ListRolePoliciesInput) (*iam.ListRolePoliciesOutput, error)
	ListAttachedRolePolicies(accountID string, input *iam.ListAttachedRolePoliciesInput) (*iam.ListAttachedRolePoliciesOutput, error)
	PutRolePolicy(accountID string, input *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error)
}

// codeStoreRole binds IAM to the one system-account code-store role, so the
// Lambda domain never holds a general IAM client.
type codeStoreRole struct {
	iam  codeStoreRoleIAM
	name string
}

var _ codestore.RoleAdmin = (*codeStoreRole)(nil)

func (r *codeStoreRole) Create(trust string, tags map[string]string) error {
	in := &iam.CreateRoleInput{
		RoleName:                 aws.String(r.name),
		AssumeRolePolicyDocument: aws.String(trust),
		Description:              aws.String("Spinifex-managed Lambda code-store role"),
	}
	for k, v := range tags {
		in.Tags = append(in.Tags, &iam.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	_, err := r.iam.CreateRole(identifiers.GlobalAccountID, in)
	if awserrors.IsErrorCode(err, awserrors.ErrorIAMEntityAlreadyExists) {
		return codestore.ErrExists
	}
	return err
}

func (r *codeStoreRole) Describe() (*codestore.RoleState, error) {
	acct, name := identifiers.GlobalAccountID, aws.String(r.name)
	got, err := r.iam.GetRole(acct, &iam.GetRoleInput{RoleName: name})
	if err != nil {
		if awserrors.IsErrorCode(err, awserrors.ErrorIAMNoSuchEntity) {
			return nil, codestore.ErrNotFound
		}
		return nil, err
	}
	tags, err := r.iam.ListRoleTags(acct, &iam.ListRoleTagsInput{RoleName: name})
	if err != nil {
		return nil, err
	}
	inline, err := r.iam.ListRolePolicies(acct, &iam.ListRolePoliciesInput{RoleName: name})
	if err != nil {
		return nil, err
	}
	attached, err := r.iam.ListAttachedRolePolicies(acct, &iam.ListAttachedRolePoliciesInput{RoleName: name})
	if err != nil {
		return nil, err
	}
	if aws.BoolValue(tags.IsTruncated) || aws.BoolValue(inline.IsTruncated) || aws.BoolValue(attached.IsTruncated) {
		return nil, errors.New("code-store role listing truncated")
	}
	st := &codestore.RoleState{
		TrustPolicy:      aws.StringValue(got.Role.AssumeRolePolicyDocument),
		Tags:             map[string]string{},
		InlinePolicies:   aws.StringValueSlice(inline.PolicyNames),
		AttachedPolicies: len(attached.AttachedPolicies),
	}
	for _, t := range tags.Tags {
		st.Tags[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}
	return st, nil
}

func (r *codeStoreRole) PutPolicy(name, doc string) error {
	_, err := r.iam.PutRolePolicy(identifiers.GlobalAccountID, &iam.PutRolePolicyInput{
		RoleName:       aws.String(r.name),
		PolicyName:     aws.String(name),
		PolicyDocument: aws.String(doc),
	})
	return err
}

// codeStoreBucket binds a system-root S3 client to the one code bucket. The
// client stays in this adapter; the Lambda domain sees only Create and Tags.
type codeStoreBucket struct {
	s3   *s3.S3
	name string
}

var _ codestore.BucketAdmin = (*codeStoreBucket)(nil)

func (b *codeStoreBucket) Create(ctx context.Context, tags map[string]string) error {
	_, err := b.s3.CreateBucketWithContext(ctx, &s3.CreateBucketInput{Bucket: aws.String(b.name)})
	if err != nil {
		if ae, ok := errors.AsType[awserr.Error](err); ok {
			switch ae.Code() {
			case s3.ErrCodeBucketAlreadyOwnedByYou:
				return codestore.ErrExists
			case s3.ErrCodeBucketAlreadyExists:
				return codestore.ErrOwnedElsewhere
			}
		}
		return err
	}
	set := &s3.Tagging{}
	for k, v := range tags {
		set.TagSet = append(set.TagSet, &s3.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	if _, err := b.s3.PutBucketTaggingWithContext(ctx, &s3.PutBucketTaggingInput{
		Bucket: aws.String(b.name), Tagging: set,
	}); err != nil {
		return fmt.Errorf("tag new code bucket: %w", err)
	}
	return nil
}

func (b *codeStoreBucket) Tags(ctx context.Context) (map[string]string, error) {
	out, err := b.s3.GetBucketTaggingWithContext(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(b.name)})
	if err != nil {
		if ae, ok := errors.AsType[awserr.Error](err); ok && ae.Code() == "NoSuchTagSet" {
			return map[string]string{}, nil
		}
		return nil, err
	}
	tags := map[string]string{}
	for _, t := range out.TagSet {
		tags[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}
	return tags, nil
}

// codeStoreAssumer is the in-process STS entry point the store's session uses.
type codeStoreAssumer interface {
	AssumeCodeStoreRole(region, sessionName string) (*sts.AssumeRoleOutput, error)
}

// codeStoreWiring is what the gateway composition root supplies.
type codeStoreWiring struct {
	Region        string
	Node          string
	Endpoint      string
	SigningRegion string
	RootAccessKey string
	RootSecretKey string
}

// newLambdaCodeStore composes the code store. The root client and root keys stay
// here; Lambda gets the bound admins for provisioning and a scoped session.
func newLambdaCodeStore(w codeStoreWiring, roles codeStoreRoleIAM, assumer codeStoreAssumer) (*codestore.Manager, error) {
	sess, err := session.NewSession(&aws.Config{
		Endpoint:         aws.String(w.Endpoint),
		Region:           aws.String(w.SigningRegion),
		Credentials:      credentials.NewStaticCredentials(w.RootAccessKey, w.RootSecretKey, ""),
		S3ForcePathStyle: aws.Bool(true),
		HTTPClient:       &http.Client{Timeout: 30 * time.Second},
	})
	if err != nil {
		return nil, fmt.Errorf("lambda code store admin session: %w", err)
	}
	role := &codeStoreRole{iam: roles, name: stsdomain.CodeStoreRoleName(w.Region)}
	bucket := &codeStoreBucket{s3: s3.New(sess), name: codestore.BucketName(w.Region)}
	sessionName := "awsgw-" + w.Node

	provision := func(ctx context.Context) error {
		return codestore.Provision(ctx, w.Region, role, bucket)
	}
	open := func() (*codestore.Store, error) {
		return codestore.Open(codestore.OpenConfig{
			Region:        w.Region,
			Endpoint:      w.Endpoint,
			SigningRegion: w.SigningRegion,
			Assume:        func() (*sts.AssumeRoleOutput, error) { return assumer.AssumeCodeStoreRole(w.Region, sessionName) },
		})
	}
	return codestore.NewManager(provision, open), nil
}
