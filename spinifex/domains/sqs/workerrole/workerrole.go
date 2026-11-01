// Package workerrole names the shared SQS worker's identity contract: the
// signing service its internal actions use, the instance role it runs under
// and the exact actions that role is granted. The gateway gate and the role
// grant both read it, so they cannot disagree.
package workerrole

// ServiceName is the SigV4 credential-scope service the SQS worker signs for.
// No AWS SDK signs for it, so no tenant request reaches these actions.
const ServiceName = "spinifex-sqs"

// RoleName is the system-account instance role the SQS worker VM runs under.
// Its trust policy must admit only EC2, so STS names every session after the
// instance and no caller can choose the session name the gate reads.
const RoleName = "sqsWorkerRole"

const (
	ActionClaimReceiptKey  = "ClaimReceiptKey"
	ActionRecordReceiptKey = "RecordReceiptKey"
	ActionPollWork         = "PollWork"
	ActionCompleteWork     = "CompleteWork"
)

// Actions lists every internal worker action, sorted.
func Actions() []string {
	return []string{ActionClaimReceiptKey, ActionCompleteWork, ActionPollWork, ActionRecordReceiptKey}
}
