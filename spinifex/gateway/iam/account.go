package gateway_iam

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// GetAccountSummary forwards to the service. The input has no required fields,
// so this is a straight pass-through.
func GetAccountSummary(accountID string, input *iam.GetAccountSummaryInput, svc handlers_iam.IAMService) (*iam.GetAccountSummaryOutput, error) {
	return svc.GetAccountSummary(accountID, input)
}

func ListAccountAliases(accountID string, input *iam.ListAccountAliasesInput, svc handlers_iam.IAMService) (*iam.ListAccountAliasesOutput, error) {
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListAccountAliases(accountID, input)
	if err != nil {
		return nil, err
	}
	out.AccountAliases, out.Marker = paginate(p, out.AccountAliases, aws.StringValue)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}
