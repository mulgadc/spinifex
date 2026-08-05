package awsmodel

import (
	"fmt"
	"sort"
	"strings"
)

// DispatchInventory is the gateway-side operation registration state supplied
// to CompareOperations.
type DispatchInventory struct {
	Registered  []string
	Stubbed     []string
	Unsupported []string
	Opaque      bool
	Note        string
}

// OperationCoverage is a deterministic model-versus-dispatch comparison.
// Implemented contains only modelled operations with a real handler; Extra
// contains registered operations absent from the pinned model.
type OperationCoverage struct {
	Service     Service
	APIVersion  string
	Modelled    []string
	Registered  []string
	Implemented []string
	Stubbed     []string
	Unsupported []string
	Missing     []string
	Extra       []string
	Opaque      bool
	Note        string
}

// CompareOperations compares one embedded service model with an authoritative
// gateway dispatch inventory.
func CompareOperations(service Service, dispatch DispatchInventory) (OperationCoverage, error) {
	model, err := Load(service)
	if err != nil {
		return OperationCoverage{}, err
	}
	coverage := OperationCoverage{
		Service:    service,
		APIVersion: model.Metadata().APIVersion,
		Modelled:   model.Operations(),
		Opaque:     dispatch.Opaque,
		Note:       dispatch.Note,
	}
	if dispatch.Opaque {
		return coverage, nil
	}

	registered, err := uniqueSorted("registered", dispatch.Registered)
	if err != nil {
		return OperationCoverage{}, fmt.Errorf("awsmodel: %s dispatch: %w", service, err)
	}
	stubbed, err := uniqueSorted("stubbed", dispatch.Stubbed)
	if err != nil {
		return OperationCoverage{}, fmt.Errorf("awsmodel: %s dispatch: %w", service, err)
	}
	unsupported, err := uniqueSorted("unsupported", dispatch.Unsupported)
	if err != nil {
		return OperationCoverage{}, fmt.Errorf("awsmodel: %s dispatch: %w", service, err)
	}
	registeredSet := toSet(registered)
	stubbedSet := toSet(stubbed)
	unsupportedSet := toSet(unsupported)
	for operation := range stubbedSet {
		if !registeredSet[operation] {
			return OperationCoverage{}, fmt.Errorf("awsmodel: %s stubbed operation %q is not registered", service, operation)
		}
		if unsupportedSet[operation] {
			return OperationCoverage{}, fmt.Errorf("awsmodel: %s operation %q is both stubbed and unsupported", service, operation)
		}
	}
	for operation := range unsupportedSet {
		if !registeredSet[operation] {
			return OperationCoverage{}, fmt.Errorf("awsmodel: %s unsupported operation %q is not registered", service, operation)
		}
	}

	modelledSet := toSet(coverage.Modelled)
	for _, operation := range coverage.Modelled {
		if !registeredSet[operation] {
			coverage.Missing = append(coverage.Missing, operation)
			continue
		}
		if !stubbedSet[operation] && !unsupportedSet[operation] {
			coverage.Implemented = append(coverage.Implemented, operation)
		}
	}
	for _, operation := range registered {
		if !modelledSet[operation] {
			coverage.Extra = append(coverage.Extra, operation)
		}
	}
	coverage.Registered = registered
	coverage.Stubbed = stubbed
	coverage.Unsupported = unsupported
	return coverage, nil
}

func uniqueSorted(label string, values []string) ([]string, error) {
	result := append([]string(nil), values...)
	sort.Strings(result)
	for index, value := range result {
		if value == "" {
			return nil, fmt.Errorf("%s operation is empty", label)
		}
		if index > 0 && result[index-1] == value {
			return nil, fmt.Errorf("%s operation %q is duplicated", label, value)
		}
	}
	return result, nil
}

func toSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

// RenderCoverageMarkdown renders a reproducible divergence report suitable for
// publishing directly or checking as a generated artefact.
func RenderCoverageMarkdown(coverages []OperationCoverage) string {
	ordered := append([]OperationCoverage(nil), coverages...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Service < ordered[j].Service })

	var report strings.Builder
	fmt.Fprintf(&report, "# AWS model operation coverage\n\n")
	fmt.Fprintf(&report, "Generated from the cached `aws-sdk-go %s` `api-2.json` models and Spinifex's authoritative gateway dispatch tables.\n\n", SourceSDKVersion)
	report.WriteString("Implemented means a modelled operation is registered to a real handler. Stub and deliberately unsupported handlers are reported separately. Shape conformance does not imply behavioural conformance.\n\n")
	report.WriteString("| Service | API version | Modelled | Registered | Implemented | Stubbed | Unsupported | Missing | Extra |\n")
	report.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, coverage := range ordered {
		if coverage.Opaque {
			fmt.Fprintf(&report, "| %s | %s | %d | — | — | — | — | — | — |\n",
				coverage.Service, coverage.APIVersion, len(coverage.Modelled))
			continue
		}
		fmt.Fprintf(&report, "| %s | %s | %d | %d | %d | %d | %d | %d | %d |\n",
			coverage.Service, coverage.APIVersion, len(coverage.Modelled), len(coverage.Registered),
			len(coverage.Implemented), len(coverage.Stubbed), len(coverage.Unsupported), len(coverage.Missing), len(coverage.Extra))
	}

	for _, coverage := range ordered {
		fmt.Fprintf(&report, "\n## %s\n\n", coverage.Service)
		if coverage.Opaque {
			fmt.Fprintf(&report, "Operation coverage is not enumerable: %s\n", coverage.Note)
			continue
		}
		percentage := float64(0)
		if len(coverage.Modelled) != 0 {
			percentage = 100 * float64(len(coverage.Implemented)) / float64(len(coverage.Modelled))
		}
		fmt.Fprintf(&report, "Implements **%d of %d** modelled operations (%.1f%%).\n", len(coverage.Implemented), len(coverage.Modelled), percentage)
		writeOperationList(&report, "Implemented", coverage.Implemented)
		writeOperationList(&report, "Missing from dispatch", coverage.Missing)
		writeOperationList(&report, "Registered stubs", coverage.Stubbed)
		writeOperationList(&report, "Deliberately unsupported", coverage.Unsupported)
		writeOperationList(&report, "Registered outside the pinned model", coverage.Extra)
		if coverage.Note != "" {
			fmt.Fprintf(&report, "\nNote: %s\n", coverage.Note)
		}
	}
	return report.String()
}

func writeOperationList(report *strings.Builder, title string, operations []string) {
	fmt.Fprintf(report, "\n<details><summary>%s (%d)</summary>\n\n", title, len(operations))
	if len(operations) == 0 {
		report.WriteString("None.\n")
	} else {
		for start := 0; start < len(operations); start += 8 {
			end := min(start+8, len(operations))
			quoted := make([]string, 0, end-start)
			for _, operation := range operations[start:end] {
				quoted = append(quoted, "`"+operation+"`")
			}
			report.WriteString(strings.Join(quoted, ", ") + "\n\n")
		}
	}
	report.WriteString("\n</details>\n")
}
