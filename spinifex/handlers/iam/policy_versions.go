package handlers_iam

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"

	iamarn "github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
)

const (
	// AWS caps a managed policy at five versions, the default included.
	maxPolicyVersions = 5

	// Terraform creates a version and prunes the oldest concurrently.
	policyCASMaxRetries = 16
)

// defaultCreatedAt is when the default version was created. Records written
// before versioning hold only v1, created with the policy.
func (p *Policy) defaultCreatedAt() string {
	if p.DefaultVersionCreatedAt != "" {
		return p.DefaultVersionCreatedAt
	}
	return p.CreatedAt
}

func (p *Policy) defaultRecord() PolicyVersionRecord {
	return PolicyVersionRecord{VersionID: p.DefaultVersion, Document: p.PolicyDocument, CreatedAt: p.defaultCreatedAt()}
}

func (p *Policy) setDefault(v PolicyVersionRecord) {
	p.DefaultVersion = v.VersionID
	p.PolicyDocument = v.Document
	p.DefaultVersionCreatedAt = v.CreatedAt
}

// versions returns every version of the policy, newest first.
func (p *Policy) versions() []PolicyVersionRecord {
	all := append([]PolicyVersionRecord{p.defaultRecord()}, p.OtherVersions...)
	slices.SortFunc(all, func(a, b PolicyVersionRecord) int {
		return cmp.Compare(versionNumber(b.VersionID), versionNumber(a.VersionID))
	})
	return all
}

// latestVersion is the highest version number issued, deleted ones included,
// so a version ID is never reused.
func (p *Policy) latestVersion() int {
	if p.LatestVersion > 0 {
		return p.LatestVersion
	}
	return versionNumber(p.versions()[0].VersionID)
}

func (p *Policy) otherVersionIndex(versionID string) int {
	return slices.IndexFunc(p.OtherVersions, func(v PolicyVersionRecord) bool { return v.VersionID == versionID })
}

// versionNumber parses "vN"; anything else sorts below every real version.
func versionNumber(versionID string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(versionID, "v"))
	if err != nil || !strings.HasPrefix(versionID, "v") {
		return 0
	}
	return n
}

func policyVersionToSDK(v PolicyVersionRecord, isDefault bool) *iam.PolicyVersion {
	return &iam.PolicyVersion{
		VersionId:        aws.String(v.VersionID),
		IsDefaultVersion: aws.Bool(isDefault),
		CreateDate:       aws.Time(parseCreatedAt(v.CreatedAt)),
	}
}

// updatePolicyCAS applies mutate to the policy named by policyARN under
// optimistic concurrency, as updateRoleCAS does for roles. mutate reports
// whether it changed the record; a false return commits nothing.
func (s *IAMServiceImpl) updatePolicyCAS(ctx context.Context, accountID, policyARN string, mutate func(*Policy) (bool, error)) error {
	key, cfg, err := policyCASTarget(accountID, policyARN)
	if err != nil {
		return err
	}
	_, err = kvutil.Update(ctx, s.policiesBucket, key, cfg, func(p *Policy) (bool, error) {
		// The key omits the path, so an ARN with the wrong path names no policy.
		if p.ARN != policyARN {
			return false, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return mutate(p)
	})
	return err
}

// policyCASTarget resolves policyARN to its KV key and the CAS settings every
// write to a policy record shares.
func policyCASTarget(accountID, policyARN string) (string, kvutil.CASConfig, error) {
	_, policyName, err := iamarn.ParsePolicyARN(policyARN)
	if err != nil {
		slog.Debug("policyCASTarget: unparseable policy ARN",
			"accountID", accountID, "policyArn", policyARN, "err", err)
		return "", kvutil.CASConfig{}, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return accountID + "." + policyName, kvutil.CASConfig{
		Attempts: policyCASMaxRetries,
		NotFound: errors.New(awserrors.ErrorIAMNoSuchEntity),
		Exhausted: func(string, int) error {
			slog.Error("IAM policy CAS retries exhausted under contention",
				"accountID", accountID, "policyName", policyName, "attempts", policyCASMaxRetries)
			return errors.New(awserrors.ErrorServerInternal)
		},
	}, nil
}

func (s *IAMServiceImpl) CreatePolicyVersion(accountID string, input *iam.CreatePolicyVersionInput) (*iam.CreatePolicyVersionOutput, error) {
	ctx := context.Background()
	if _, err := ValidatePolicyDocument(*input.PolicyDocument); err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorIAMMalformedPolicyDocument,
			"policy %q: %w", *input.PolicyArn, err)
	}
	setAsDefault := aws.BoolValue(input.SetAsDefault)

	var created PolicyVersionRecord
	err := s.updatePolicyCAS(ctx, accountID, *input.PolicyArn, func(p *Policy) (bool, error) {
		if 1+len(p.OtherVersions) >= maxPolicyVersions {
			return false, awserrors.Errorf(awserrors.ErrorIAMLimitExceeded,
				"A managed policy can have up to %d versions. Before you create a new version, you must delete an existing version.", maxPolicyVersions)
		}
		n := p.latestVersion() + 1
		created = PolicyVersionRecord{
			VersionID: fmt.Sprintf("v%d", n),
			Document:  *input.PolicyDocument,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		p.LatestVersion = n
		if setAsDefault {
			p.OtherVersions = append(p.OtherVersions, p.defaultRecord())
			p.setDefault(created)
			p.UpdatedAt = created.CreatedAt
		} else {
			p.OtherVersions = append(p.OtherVersions, created)
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM policy version created", "accountID", accountID, "policyArn", *input.PolicyArn,
		"versionId", created.VersionID, "setAsDefault", setAsDefault)
	return &iam.CreatePolicyVersionOutput{PolicyVersion: policyVersionToSDK(created, setAsDefault)}, nil
}

func (s *IAMServiceImpl) SetDefaultPolicyVersion(accountID string, input *iam.SetDefaultPolicyVersionInput) (*iam.SetDefaultPolicyVersionOutput, error) {
	ctx := context.Background()
	versionID := *input.VersionId
	err := s.updatePolicyCAS(ctx, accountID, *input.PolicyArn, func(p *Policy) (bool, error) {
		if p.DefaultVersion == versionID {
			return false, nil
		}
		i := p.otherVersionIndex(versionID)
		if i < 0 {
			return false, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		incoming := p.OtherVersions[i]
		p.OtherVersions[i] = p.defaultRecord()
		p.setDefault(incoming)
		p.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM policy default version set", "accountID", accountID, "policyArn", *input.PolicyArn, "versionId", versionID)
	return &iam.SetDefaultPolicyVersionOutput{}, nil
}

func (s *IAMServiceImpl) DeletePolicyVersion(accountID string, input *iam.DeletePolicyVersionInput) (*iam.DeletePolicyVersionOutput, error) {
	ctx := context.Background()
	versionID := *input.VersionId
	err := s.updatePolicyCAS(ctx, accountID, *input.PolicyArn, func(p *Policy) (bool, error) {
		if p.DefaultVersion == versionID {
			return false, awserrors.Errorf(awserrors.ErrorIAMDeleteConflict, "Cannot delete the default version of a policy.")
		}
		i := p.otherVersionIndex(versionID)
		if i < 0 {
			return false, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		p.LatestVersion = p.latestVersion()
		p.OtherVersions = slices.Delete(p.OtherVersions, i, i+1)
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM policy version deleted", "accountID", accountID, "policyArn", *input.PolicyArn, "versionId", versionID)
	return &iam.DeletePolicyVersionOutput{}, nil
}

func (s *IAMServiceImpl) GetPolicyVersion(accountID string, input *iam.GetPolicyVersionInput) (*iam.GetPolicyVersionOutput, error) {
	ctx := context.Background()
	policy, err := s.getPolicyByARN(ctx, accountID, *input.PolicyArn)
	if err != nil {
		return nil, err
	}

	for _, v := range policy.versions() {
		if v.VersionID == *input.VersionId {
			out := policyVersionToSDK(v, v.VersionID == policy.DefaultVersion)
			out.Document = aws.String(v.Document)
			return &iam.GetPolicyVersionOutput{PolicyVersion: out}, nil
		}
	}
	return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
}

// ListPolicyVersions returns every version newest first, without documents, as
// AWS does. Returns the whole list; the gateway pages it.
func (s *IAMServiceImpl) ListPolicyVersions(accountID string, input *iam.ListPolicyVersionsInput) (*iam.ListPolicyVersionsOutput, error) {
	ctx := context.Background()
	policy, err := s.getPolicyByARN(ctx, accountID, *input.PolicyArn)
	if err != nil {
		return nil, err
	}

	versions := policy.versions()
	out := make([]*iam.PolicyVersion, 0, len(versions))
	for _, v := range versions {
		out = append(out, policyVersionToSDK(v, v.VersionID == policy.DefaultVersion))
	}
	return &iam.ListPolicyVersionsOutput{Versions: out, IsTruncated: aws.Bool(false)}, nil
}
