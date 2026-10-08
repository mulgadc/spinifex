package addon

import (
	"fmt"
	"maps"
	"slices"
)

// NvidiaDevicePlugin is staged by a GPU node group's launch (EnsureGPUDevicePlugin),
// never user-requested directly.
const NvidiaDevicePlugin = "nvidia-device-plugin"

// Spec describes one Spinifex-supported managed add-on in the static in-binary
// catalogue. DescribeAddonVersions and CreateAddon validate against it.
type Spec struct {
	// Name is the AWS add-on name (e.g. "aws-load-balancer-controller").
	Name string
	// Versions lists supported versions newest-first; [0] is the default.
	Versions       []string
	DefaultVersion string
	// RequiresIRSA indicates the add-on needs an IAM role for service-account binding.
	RequiresIRSA bool
	Description  string
	// Hidden keeps the spec out of the unfiltered catalogue listing while it
	// stays creatable by name; used for internal fixtures.
	Hidden bool
}

// catalog is the bundled add-on registry. Keep newest version first per slice.
var catalog = buildCatalog(
	newSpec("aws-load-balancer-controller", true,
		"Provisions ELBv2 load balancers for Kubernetes Service/Ingress resources.",
		"2.11.0"),
	newSpec("argocd", false,
		"Declarative GitOps continuous delivery for Kubernetes.",
		"3.0.23"),
	newSpec("aws-ebs-csi-driver", true,
		"Container Storage Interface driver for Amazon EBS (Viperblock) volumes.",
		"1.40.1"),
	// Staged automatically for GPU node groups, so hidden from the public
	// catalogue like spinifex-noop but still creatable by name.
	hidden(newSpec(NvidiaDevicePlugin, false,
		"NVIDIA device plugin exposing nvidia.com/gpu allocatable via CDI on GPU-tainted nodes.",
		"0.17.4")),
	// spinifex-noop is the delivery-transport fixture: a trivial bundle
	// (Namespace + ConfigMap) the add-on e2e uses to prove stage, render,
	// auto-deploy, ACTIVE and delete round-trip without a real workload.
	hidden(newSpec("spinifex-noop", false,
		"No-op delivery-transport fixture (Namespace + ConfigMap).",
		"0.1.0")),
)

// hidden marks a spec as internal: still creatable, but absent from the
// unfiltered catalogue listing.
func hidden(s Spec) Spec {
	s.Hidden = true
	return s
}

// newSpec builds a spec with the first version as its default.
func newSpec(name string, requiresIRSA bool, description string, versions ...string) Spec {
	spec := Spec{
		Name:         name,
		Versions:     versions,
		RequiresIRSA: requiresIRSA,
		Description:  description,
	}
	if len(versions) > 0 {
		spec.DefaultVersion = versions[0]
	}
	return spec
}

// buildCatalog indexes the specs by name.
func buildCatalog(specs ...Spec) map[string]Spec {
	out := make(map[string]Spec, len(specs))
	for _, s := range specs {
		out[s.Name] = s
	}
	return out
}

// ValidateCatalog checks the bundled catalogue is internally consistent.
func ValidateCatalog() error {
	return validateCatalog(catalog)
}

func validateCatalog(c map[string]Spec) error {
	for name, spec := range c {
		if spec.Name != name {
			return fmt.Errorf("add-on catalog key %q does not match spec name %q", name, spec.Name)
		}
		if len(spec.Versions) == 0 {
			return fmt.Errorf("add-on %q has no versions", name)
		}
		if !spec.SupportsVersion(spec.DefaultVersion) {
			return fmt.Errorf("add-on %q default version %q is not supported", name, spec.DefaultVersion)
		}
	}
	return nil
}

// Lookup returns the spec for name and whether it is in the catalogue.
func Lookup(name string) (Spec, bool) {
	spec, ok := catalog[name]
	return spec, ok
}

// SupportsVersion reports whether the spec lists the given version.
func (s Spec) SupportsVersion(version string) bool {
	return slices.Contains(s.Versions, version)
}

// Specs returns every catalogue entry, hidden ones included, sorted by name.
func Specs() []Spec {
	names := slices.Sorted(maps.Keys(catalog))
	out := make([]Spec, 0, len(names))
	for _, n := range names {
		out = append(out, catalog[n])
	}
	return out
}
