package gpu

// GPUAttachment describes how a GPU is attached to a VM instance.
// Exactly one of PCIAddress or MdevPath is non-empty.
type GPUAttachment struct {
	PCIAddress  string // non-empty for whole-GPU VFIO passthrough
	MdevPath    string // non-empty for MIG mdev passthrough
	XVGAEnabled bool   // meaningful only for whole-GPU; MIG is always headless
}
