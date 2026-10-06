package handlers_elbv2

import "github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"

// These aliases re-export systeminstance types so ELBv2 and EKS share launch
// logic without an eks→elbv2 dependency. ELBv2 always uses BootMode=BootDirect.
type (
	SystemInstanceLauncher = systeminstance.SystemInstanceLauncher
	SystemInstanceInput    = systeminstance.SystemInstanceInput
	ExtraENIInput          = systeminstance.ExtraENIInput
	NICConfig              = systeminstance.NICConfig
	RecoveryContext        = systeminstance.RecoveryContext
	SystemInstanceOutput   = systeminstance.SystemInstanceOutput
)
