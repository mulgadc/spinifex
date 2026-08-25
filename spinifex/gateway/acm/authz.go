package gateway_acm

import (
	"errors"
	"log/slog"
	"sort"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gateway/bodyscope"
)

// The resource a policy check evaluates against when the request names nothing
// in particular, or when the identifier cannot be resolved at gate time.
const anyResource = "*"

// A created certificate has no id until the handler mints one. This is what AWS
// evaluates a create against, and it is a value rather than a pattern, so it
// matches a policy scoped to the type without widening a grant.
const anyCertificate = "*"

// Where an action's certificate is named in the JSON body.
type resourceSource uint8

const (
	sourceAny resourceSource = iota
	sourceCertificate
	sourceNewCertificate
)

// Every action ACM_Request serves, with the resource AWS evaluates it against.
// Exhaustive by contract: a completeness test compares this table with the
// dispatch table in both directions, so an action cannot be added with a silent
// account-wide grant.
//
// One source per action rather than a list: ACM has a single resource type and
// no action AWS evaluates against several certificates.
var acmScopes = map[string]resourceSource{
	"DescribeCertificate":       sourceCertificate,
	"GetCertificate":            sourceCertificate,
	"DeleteCertificate":         sourceCertificate,
	"ListTagsForCertificate":    sourceCertificate,
	"AddTagsToCertificate":      sourceCertificate,
	"RemoveTagsFromCertificate": sourceCertificate,

	// CertificateArn is optional: present means re-import into an existing
	// certificate, absent means create a new one.
	"ImportCertificate": sourceNewCertificate,

	// Issues a certificate; no id exists at gate time.
	"RequestCertificate": sourceNewCertificate,

	// Account-level. AWS documents no resource type for the certificate list.
	"ListCertificates": sourceAny,
}

// HasScope reports whether action has an explicit ACM scope-table entry.
// acmActions lives in package gateway, so the completeness test calls this
// rather than reaching into the table.
func HasScope(action string) bool {
	_, ok := acmScopes[action]
	return ok
}

// ScopedActions returns every action represented in the ACM scope table.
func ScopedActions() []string {
	actions := make([]string, 0, len(acmScopes))
	for action := range acmScopes {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions
}

// ResourceARNs resolves the resources an ACM request authorizes against from the
// same body bytes the handler will unmarshal. The region and account a
// caller-supplied CertificateArn carries are ignored: the handler only ever acts
// on a certificate in the caller's own account, and honouring the caller's
// anchor would let a request slide out from under a Deny scoped to the real one.
func ResourceARNs(action, region, accountID string, body []byte) ([]string, error) {
	source, ok := acmScopes[action]
	if !ok {
		slog.Error("ACM authz: action is served but absent from the scope table", "action", action)
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}

	scope, err := bodyscope.Parse(action, body)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	switch source {
	case sourceAny:
		return []string{anyResource}, nil

	case sourceCertificate:
		return []string{certificateARN(region, accountID, scope.String("certificateArn"))}, nil

	case sourceNewCertificate:
		// An absent identifier means the request creates a certificate, so the
		// create form applies rather than the account-wide fallback.
		if !scope.Has("certificateArn") {
			return []string{newCertificateARN(region, accountID)}, nil
		}
		return []string{certificateARN(region, accountID, scope.String("certificateArn"))}, nil

	default:
		slog.Error("ACM authz: unhandled resource source, failing closed", "action", action, "source", source)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
}

// certificateARN re-anchors the caller-supplied ARN on gw.Region and the
// caller's account. An absent or unreadable identifier authorizes account-wide,
// so a malformed request stays the handler's validation fault rather than
// becoming an authorization failure.
func certificateARN(region, accountID, certARN string) string {
	if region == "" || accountID == "" {
		return anyResource
	}
	id, ok := arn.ParseACMCertificateID(certARN)
	if !ok {
		return anyResource
	}
	return arn.FormatACMCertificate(region, accountID, id)
}

func newCertificateARN(region, accountID string) string {
	if region == "" || accountID == "" {
		return anyResource
	}
	return arn.FormatACMCertificate(region, accountID, anyCertificate)
}
