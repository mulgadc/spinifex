package handlers_eks

import "slices"

// nvidiaDevicePluginAddonName is auto-staged on GPU nodegroup create (see
// stageGPUDeviceAddon in nodegroup.go), never user-requested directly.
const nvidiaDevicePluginAddonName = "nvidia-device-plugin"

// AddonSpec describes one Spinifex-supported managed add-on in the static
// in-binary catalog. DescribeAddonVersions and CreateAddon validate against it.
type AddonSpec struct {
	// Name is the AWS add-on name (e.g. "aws-load-balancer-controller").
	Name string
	// Versions lists supported versions newest-first; [0] is the default.
	Versions       []string
	DefaultVersion string
	// RequiresIRSA indicates the add-on needs an IAM role for service-account binding.
	RequiresIRSA bool
	Description  string
	// Hidden keeps the spec out of the unfiltered DescribeAddonVersions listing.
	// It stays creatable (lookupAddon finds it) — used for internal fixtures that
	// should not surface as user-installable add-ons.
	Hidden bool
}

// addonCatalog is the bundled add-on registry. Keep newest version first per slice.
var addonCatalog = buildAddonCatalog(
	newAddonSpec("aws-load-balancer-controller", true,
		"Provisions ELBv2 load balancers for Kubernetes Service/Ingress resources.",
		"2.11.0"),
	newAddonSpec("argocd", false,
		"Declarative GitOps continuous delivery for Kubernetes.",
		"3.0.23"),
	newAddonSpec("aws-ebs-csi-driver", true,
		"Container Storage Interface driver for Amazon EBS (Viperblock) volumes.",
		"1.40.1"),
	// nvidia-device-plugin is auto-staged (never user-requested) on GPU
	// nodegroup create, so it is hidden from the public catalog like
	// spinifex-noop but still creatable by name.
	hiddenAddonSpec(newAddonSpec(nvidiaDevicePluginAddonName, false,
		"NVIDIA device plugin exposing nvidia.com/gpu allocatable via CDI on GPU-tainted nodes.",
		"0.17.4")),
	// spinifex-noop is the delivery-transport fixture: a trivial bundle
	// (Namespace + ConfigMap) used by the addon e2e to prove stage → render →
	// auto-deploy → ACTIVE → delete round-trips end-to-end without depending on
	// the CSI or load-balancer-controller manifests. Not a real workload addon,
	// so it is hidden from the public DescribeAddonVersions catalog.
	hiddenAddonSpec(newAddonSpec("spinifex-noop", false,
		"No-op delivery-transport fixture (Namespace + ConfigMap).",
		"0.1.0")),
)

// hiddenAddonSpec marks a spec as internal: still creatable, but absent from the
// unfiltered add-on catalog listing.
func hiddenAddonSpec(s AddonSpec) AddonSpec {
	s.Hidden = true
	return s
}

// newAddonSpec builds a spec with DefaultVersion = versions[0]. Panics on empty versions.
func newAddonSpec(name string, requiresIRSA bool, description string, versions ...string) AddonSpec {
	if len(versions) == 0 {
		panic("eks: addon spec " + name + " has no versions")
	}
	return AddonSpec{
		Name:           name,
		Versions:       versions,
		DefaultVersion: versions[0],
		RequiresIRSA:   requiresIRSA,
		Description:    description,
	}
}

// buildAddonCatalog indexes the specs by name.
func buildAddonCatalog(specs ...AddonSpec) map[string]AddonSpec {
	out := make(map[string]AddonSpec, len(specs))
	for _, s := range specs {
		out[s.Name] = s
	}
	return out
}

// lookupAddon returns the spec for name and whether it is in the catalog.
func lookupAddon(name string) (AddonSpec, bool) {
	spec, ok := addonCatalog[name]
	return spec, ok
}

// supportsVersion reports whether the spec lists the given version.
func (s AddonSpec) supportsVersion(version string) bool {
	return slices.Contains(s.Versions, version)
}

// catalogSpecs returns the catalog entries sorted by name for stable output.
func catalogSpecs() []AddonSpec {
	names := make([]string, 0, len(addonCatalog))
	for n := range addonCatalog {
		names = append(names, n)
	}
	slices.Sort(names)
	out := make([]AddonSpec, 0, len(names))
	for _, n := range names {
		out = append(out, addonCatalog[n])
	}
	return out
}
