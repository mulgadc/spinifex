package handlers_elbv2

import "github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"

// The system-instance launch contract now lives in the boot-agnostic
// handlers/sysinstance package so both ELBv2 and EKS can launch system VMs
// without an eks→elbv2 dependency. These aliases keep existing ELBv2 source
// (which builds direct-boot microVM inputs) compiling unchanged. ELBv2 always
// launches with BootMode=BootDirect (the zero value).
type (
	SystemInstanceLauncher = sysinstance.SystemInstanceLauncher
	SystemInstanceInput    = sysinstance.SystemInstanceInput
	ExtraENIInput          = sysinstance.ExtraENIInput
	NICConfig              = sysinstance.NICConfig
	RecoveryContext        = sysinstance.RecoveryContext
	SystemInstanceOutput   = sysinstance.SystemInstanceOutput
)
