package handlers_eks

import "slices"

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
}

// addonCatalog is the bundled add-on registry. Keep newest version first per slice.
var addonCatalog = buildAddonCatalog(
	newAddonSpec("aws-load-balancer-controller", true,
		"Provisions ELBv2 load balancers for Kubernetes Service/Ingress resources.",
		"2.8.1"),
	newAddonSpec("coredns", false,
		"Cluster DNS server.",
		"1.11.1"),
	newAddonSpec("argocd", false,
		"Declarative GitOps continuous delivery for Kubernetes.",
		"3.0.23"),
	// spinifex-noop is the delivery-transport fixture: a trivial bundle
	// (Namespace + ConfigMap) used by the addon e2e to prove stage → render →
	// auto-deploy → ACTIVE → delete round-trips end-to-end without depending on
	// the CSI or load-balancer-controller manifests. Not a real workload addon.
	newAddonSpec("spinifex-noop", false,
		"No-op delivery-transport fixture (Namespace + ConfigMap).",
		"0.1.0"),
)

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
	insertionSortStrings(names)
	out := make([]AddonSpec, 0, len(names))
	for _, n := range names {
		out = append(out, addonCatalog[n])
	}
	return out
}

func insertionSortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
