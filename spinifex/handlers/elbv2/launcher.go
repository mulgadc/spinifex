package handlers_elbv2

import "github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"

// These aliases re-export sysinstance types so ELBv2 and EKS share launch
// logic without an eks→elbv2 dependency. ELBv2 always uses BootMode=BootDirect.
type (
	SystemInstanceLauncher = sysinstance.SystemInstanceLauncher
	SystemInstanceInput    = sysinstance.SystemInstanceInput
	ExtraENIInput          = sysinstance.ExtraENIInput
	NICConfig              = sysinstance.NICConfig
	RecoveryContext        = sysinstance.RecoveryContext
	SystemInstanceOutput   = sysinstance.SystemInstanceOutput
)
