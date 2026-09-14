package gateway_sts

import (
	"github.com/aws/aws-sdk-go/service/sts"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
)

// GetAccessKeyInfo delegates to the STSService; not gated by caller IAM policy,
// as AWS requires no permission for this call. No caller fields are passed: the
// answer is not scoped to the caller, so a key owned by another account resolves.
func GetAccessKeyInfo(input *sts.GetAccessKeyInfoInput, svc handlers_sts.STSService) (*sts.GetAccessKeyInfoOutput, error) {
	return svc.GetAccessKeyInfo(input)
}
