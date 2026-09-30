//go:build integration

package integration

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/mulgadc/spinifex/internal/awsmodel"
)

type conformanceMode string

const (
	conformanceModeWarn conformanceMode = "warn"
	conformanceModeFail conformanceMode = "fail"
)

// conformancePolicy lists the services whose findings block the suite: their
// responses, and separately the generated requests they are sent.
type conformancePolicy struct {
	promoted        map[awsmodel.Service]bool
	requestPromoted map[awsmodel.Service]bool
}

type conformancePolicyFile struct {
	PromotedServices        []awsmodel.Service `json:"promotedServices"`
	PromotedRequestServices []awsmodel.Service `json:"promotedRequestServices"`
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

	promoted, err := promotedServiceSet("promotedServices", file.PromotedServices)
	if err != nil {
		return conformancePolicy{}, err
	}
	requestPromoted, err := promotedServiceSet("promotedRequestServices", file.PromotedRequestServices)
	if err != nil {
		return conformancePolicy{}, err
	}
	return conformancePolicy{promoted: promoted, requestPromoted: requestPromoted}, nil
}

func promotedServiceSet(field string, services []awsmodel.Service) (map[awsmodel.Service]bool, error) {
	known := make(map[awsmodel.Service]bool)
	for _, service := range awsmodel.Services() {
		known[service] = true
	}
	set := make(map[awsmodel.Service]bool, len(services))
	for _, service := range services {
		if !known[service] {
			return nil, fmt.Errorf("promoted-services policy %s contains unknown service %q", field, service)
		}
		if set[service] {
			return nil, fmt.Errorf("promoted-services policy %s contains duplicate service %q", field, service)
		}
		set[service] = true
	}
	return set, nil
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

func (p conformancePolicy) isRequestPromoted(service awsmodel.Service) bool {
	return p.requestPromoted[service]
}

func (p conformancePolicy) services() []awsmodel.Service {
	return slices.Sorted(maps.Keys(p.promoted))
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
