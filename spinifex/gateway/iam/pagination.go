package gateway_iam

import (
	"cmp"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// AWS's page size when MaxItems is omitted, and the largest MaxItems it accepts.
const (
	defaultMaxItems = 100
	maxMaxItems     = 1000
)

// pager is a validated Marker/MaxItems pair. Pages are cut in the gateway, after
// the service has filtered, so in-process callers of the service still see the
// whole list.
type pager struct {
	after    string
	resuming bool
	size     int
}

// newPager validates the request before the service runs, so a bad Marker or
// MaxItems fails the call whatever the listed entity's state.
func newPager(marker *string, maxItems *int64) (pager, error) {
	p := pager{size: defaultMaxItems}
	if maxItems != nil {
		n := *maxItems
		if n < 1 {
			return pager{}, awserrors.Errorf(awserrors.ErrorValidationError,
				"1 validation error detected: Value '%d' at 'maxItems' failed to satisfy constraint: Member must have value greater than or equal to 1", n)
		}
		if n > maxMaxItems {
			return pager{}, awserrors.Errorf(awserrors.ErrorValidationError,
				"1 validation error detected: Value '%d' at 'maxItems' failed to satisfy constraint: Member must have value less than or equal to %d", n, maxMaxItems)
		}
		p.size = int(n)
	}
	if marker != nil {
		after, err := base64.RawURLEncoding.DecodeString(*marker)
		if err != nil || len(after) == 0 {
			return pager{}, awserrors.Errorf(awserrors.ErrorValidationError, "Invalid Marker.")
		}
		p.after, p.resuming = string(after), true
	}
	return p, nil
}

// paginate sorts items by key and returns the page after the marker, with the
// marker for the next page, nil when this page is the last.
func paginate[T any](p pager, items []T, key func(T) string) ([]T, *string) {
	return paginateFunc(p, items, key, strings.Compare)
}

// paginateFunc is paginate for a list whose order is not ascending by key. The
// marker holds the last key returned rather than an offset, so a page boundary
// survives entries being added or removed between calls.
func paginateFunc[T any](p pager, items []T, key func(T) string, cmp func(a, b string) int) ([]T, *string) {
	slices.SortStableFunc(items, func(a, b T) int { return cmp(key(a), key(b)) })
	start := 0
	if p.resuming {
		i, found := slices.BinarySearchFunc(items, p.after, func(item T, after string) int { return cmp(key(item), after) })
		if found {
			i++
		}
		start = i
	}
	end := min(start+p.size, len(items))
	page := items[start:end]
	if end == len(items) {
		return page, nil
	}
	return page, aws.String(base64.RawURLEncoding.EncodeToString([]byte(key(page[len(page)-1]))))
}

// Sort keys. A managed policy's name is unique only within its account, so the
// ARN breaks the tie between a customer policy and an AWS one of the same name.
func userKey(u *iam.User) string                       { return aws.StringValue(u.UserName) }
func roleKey(r *iam.Role) string                       { return aws.StringValue(r.RoleName) }
func groupKey(g *iam.Group) string                     { return aws.StringValue(g.GroupName) }
func tagKey(t *iam.Tag) string                         { return aws.StringValue(t.Key) }
func accessKeyKey(k *iam.AccessKeyMetadata) string     { return aws.StringValue(k.AccessKeyId) }
func instanceProfileKey(p *iam.InstanceProfile) string { return aws.StringValue(p.InstanceProfileName) }
func policyKey(p *iam.Policy) string {
	return aws.StringValue(p.PolicyName) + "\x00" + aws.StringValue(p.Arn)
}
func attachedPolicyKey(p *iam.AttachedPolicy) string {
	return aws.StringValue(p.PolicyName) + "\x00" + aws.StringValue(p.PolicyArn)
}

func policyVersionKey(v *iam.PolicyVersion) string { return aws.StringValue(v.VersionId) }

// newestVersionFirst orders version IDs (v1, v2, ...) by descending number, the
// order ListPolicyVersions returns them in.
func newestVersionFirst(a, b string) int {
	return cmp.Compare(versionNumber(b), versionNumber(a))
}

func versionNumber(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "v"))
	return n
}
