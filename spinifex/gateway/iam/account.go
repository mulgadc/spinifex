package gateway_iam

import (
	"github.com/aws/aws-sdk-go/service/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// GetAccountSummary forwards to the service. The input has no required fields,
// so this is a straight pass-through.
func GetAccountSummary(accountID string, input *iam.GetAccountSummaryInput, svc handlers_iam.IAMService) (*iam.GetAccountSummaryOutput, error) {
	return svc.GetAccountSummary(accountID, input)
}
