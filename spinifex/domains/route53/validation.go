package route53

import (
	"fmt"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// Length bounds the Route 53 model puts on its string shapes. AWS refuses a
// request that breaks one with InvalidInput before it looks anything up.
const (
	maxResourceIDLength = 32
	maxPageMarkerLength = 64
	maxDNSNameLength    = 1024
	maxRDataLength      = 4000
)

// rrTypes is RRType as the current Route 53 model lists it; the v1 SDK's list
// predates TLSA, SSHFP, SVCB and HTTPS.
var rrTypes = append(route53.RRType_Values(), "TLSA", "SSHFP", "SVCB", "HTTPS")

func invalidMember(field, value, constraint string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidInput,
		"1 validation error detected: Value %s at '%s' failed to satisfy constraint: Member must %s", value, field, constraint)
}

func requireMember(field string, value *string) error {
	if value == nil {
		return invalidMember(field, "null", "not be null")
	}
	return nil
}

func checkLength(field string, value *string, maxLen int) error {
	if v := aws.StringValue(value); len(v) > maxLen {
		return invalidMember(field, "'"+v+"'", fmt.Sprintf("have length less than or equal to %d", maxLen))
	}
	return nil
}

func checkEnum(field string, value *string, allowed []string) error {
	if value != nil && !slices.Contains(allowed, *value) {
		return invalidMember(field, "'"+*value+"'", "satisfy enum value set: ["+strings.Join(allowed, ", ")+"]")
	}
	return nil
}

// hostedZoneID returns the bare ID a request names, refusing one the model's
// ResourceId shape cannot hold.
func hostedZoneID(field string, raw *string) (string, error) {
	id := BareZoneID(aws.StringValue(raw))
	if id == "" {
		return "", awserrors.Errorf(awserrors.ErrorInvalidInput, "a hosted zone ID is required")
	}
	if err := checkLength(field, &id, maxResourceIDLength); err != nil {
		return "", err
	}
	return id, nil
}

// checkChangeBatchShape refuses a change that breaks the model's own bounds.
// What remains for applyChangeBatch are the semantic InvalidChangeBatch errors.
func checkChangeBatchShape(batch *route53.ChangeBatch) error {
	if batch == nil {
		return nil
	}
	for i, ch := range batch.Changes {
		if ch == nil {
			continue
		}
		at := fmt.Sprintf("changeBatch.changes.%d.member", i+1)
		if err := requireMember(at+".action", ch.Action); err != nil {
			return err
		}
		if err := checkEnum(at+".action", ch.Action, route53.ChangeAction_Values()); err != nil {
			return err
		}
		set := ch.ResourceRecordSet
		if set == nil {
			continue
		}
		if err := requireMember(at+".resourceRecordSet.name", set.Name); err != nil {
			return err
		}
		if err := checkLength(at+".resourceRecordSet.name", set.Name, maxDNSNameLength); err != nil {
			return err
		}
		for j, rr := range set.ResourceRecords {
			if rr == nil {
				continue
			}
			field := fmt.Sprintf("%s.resourceRecordSet.resourceRecords.%d.member.value", at, j+1)
			if err := requireMember(field, rr.Value); err != nil {
				return err
			}
			if err := checkLength(field, rr.Value, maxRDataLength); err != nil {
				return err
			}
		}
	}
	return nil
}
