package gpu

// GPUAttachment describes how a GPU is attached to a VM instance.
// Exactly one of PCIAddress or MdevPath is non-empty.
type GPUAttachment struct {
	PCIAddress  string `json:"pci_address,omitempty"`
	MdevPath    string `json:"mdev_path,omitempty"`
	XVGAEnabled bool   `json:"xvga_enabled,omitempty"`
}
