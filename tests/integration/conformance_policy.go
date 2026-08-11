//go:build integration

package integration

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mulgadc/spinifex/internal/awsmodel"
)

type conformanceMode string

const (
	conformanceModeWarn conformanceMode = "warn"
	conformanceModeFail conformanceMode = "fail"
)

type conformancePolicy struct {
	promoted map[awsmodel.Service]bool
}

type conformancePolicyFile struct {
	PromotedServices []awsmodel.Service `json:"promotedServices"`
}

//go:embed conformance-promoted-services.json
var conformancePolicyJSON []byte

func loadConformancePolicy() (conformancePolicy, error) {
	decoder := json.NewDecoder(strings.NewReader(string(conformancePolicyJSON)))
	decoder.DisallowUnknownFields()
	var file conformancePolicyFile
	if err := decoder.Decode(&file); err != nil {
		return conformancePolicy{}, fmt.Errorf("parse promoted-services policy: %w", err)
	}

	known := make(map[awsmodel.Service]bool)
	for _, service := range awsmodel.Services() {
		known[service] = true
	}
	policy := conformancePolicy{promoted: make(map[awsmodel.Service]bool, len(file.PromotedServices))}
	for _, service := range file.PromotedServices {
		if !known[service] {
			return conformancePolicy{}, fmt.Errorf("promoted-services policy contains unknown service %q", service)
		}
		if policy.promoted[service] {
			return conformancePolicy{}, fmt.Errorf("promoted-services policy contains duplicate service %q", service)
		}
		policy.promoted[service] = true
	}
	return policy, nil
}

func conformancePolicyFor(services ...awsmodel.Service) conformancePolicy {
	policy := conformancePolicy{promoted: make(map[awsmodel.Service]bool, len(services))}
	for _, service := range services {
		policy.promoted[service] = true
	}
	return policy
}

func (p conformancePolicy) isPromoted(service awsmodel.Service) bool {
	return p.promoted[service]
}

func (p conformancePolicy) services() []awsmodel.Service {
	services := make([]awsmodel.Service, 0, len(p.promoted))
	for service := range p.promoted {
		services = append(services, service)
	}
	sort.Slice(services, func(i, j int) bool { return services[i] < services[j] })
	return services
}

func conformanceModeFromEnvironment() (conformanceMode, error) {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("AWS_MODEL_CONFORMANCE_MODE")))
	if value == "" {
		return conformanceModeFail, nil
	}
	switch conformanceMode(value) {
	case conformanceModeWarn, conformanceModeFail:
		return conformanceMode(value), nil
	default:
		return "", fmt.Errorf("AWS_MODEL_CONFORMANCE_MODE must be %q or %q, got %q", conformanceModeWarn, conformanceModeFail, value)
	}
}
