package handlers_ec2_launchtemplate

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// The launch-template type families (RequestLaunchTemplateData,
// ResponseLaunchTemplateData, RunInstancesInput) share top-level field names but
// use different nested Go types. The SDK structs carry no json tags, so a json
// round-trip keys off Go field names and transfers every identically-named field,
// preserving nil vs non-nil-empty slices. The only names that diverge, and thus
// must be handled explicitly, are enumerated in responseToRunInstances below.

// requestToResponse converts create-time RequestLaunchTemplateData into the stored
// ResponseLaunchTemplateData. The two families have identical field-name sets, so
// the round-trip is lossless.
func requestToResponse(in *ec2.RequestLaunchTemplateData) (*ec2.ResponseLaunchTemplateData, error) {
	if in == nil {
		return nil, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal RequestLaunchTemplateData: %w", err)
	}
	out := &ec2.ResponseLaunchTemplateData{}
	if err := json.Unmarshal(b, out); err != nil {
		return nil, fmt.Errorf("unmarshal into ResponseLaunchTemplateData: %w", err)
	}
	return out, nil
}

// responseToRunInstances expands a stored template version into a RunInstancesInput.
// The round-trip handles every identically-named field; the three fields whose Go
// names diverge from RunInstancesInput are handled explicitly: RamDiskId->RamdiskId
// and ElasticGpuSpecifications->ElasticGpuSpecification are remapped, and
// InstanceRequirements (no RunInstances equivalent) is intentionally dropped.
// MinCount/MaxCount are never sourced from a template.
func responseToRunInstances(in *ec2.ResponseLaunchTemplateData) (*ec2.RunInstancesInput, error) {
	if in == nil {
		return nil, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal ResponseLaunchTemplateData: %w", err)
	}
	out := &ec2.RunInstancesInput{}
	if err := json.Unmarshal(b, out); err != nil {
		return nil, fmt.Errorf("unmarshal into RunInstancesInput: %w", err)
	}
	out.RamdiskId = in.RamDiskId
	if in.ElasticGpuSpecifications != nil {
		out.ElasticGpuSpecification = make([]*ec2.ElasticGpuSpecification, len(in.ElasticGpuSpecifications))
		for i, e := range in.ElasticGpuSpecifications {
			if e != nil {
				out.ElasticGpuSpecification[i] = &ec2.ElasticGpuSpecification{Type: e.Type}
			}
		}
	}
	return out, nil
}

// mergeResponseData overlays override onto base with whole-field, presence-based
// replace (used by CreateLaunchTemplateVersion's SourceVersion clone-and-override).
func mergeResponseData(base, override *ec2.ResponseLaunchTemplateData) *ec2.ResponseLaunchTemplateData {
	if base == nil {
		base = &ec2.ResponseLaunchTemplateData{}
	}
	out := *base
	if override != nil {
		mergeWholeField(&out, override)
	}
	return &out
}

// mergeRunInstancesInput overlays direct RunInstances params (override) onto a
// template-derived base. Because a template never carries MinCount/MaxCount/
// ClientToken/DryRun/LaunchTemplate, those come from the direct request alone.
func mergeRunInstancesInput(base, override *ec2.RunInstancesInput) *ec2.RunInstancesInput {
	if base == nil {
		base = &ec2.RunInstancesInput{}
	}
	out := *base
	if override != nil {
		mergeWholeField(&out, override)
	}
	return &out
}

// mergeWholeField overlays override onto base in place: for each exported field a
// "present" override value (non-nil pointer/slice/map, incl. a non-nil empty slice
// that honours an explicit clear) replaces the base field entirely; a nil override
// field inherits base. base and override must be non-nil pointers to the same
// struct type, so there is no field-name drift risk.
func mergeWholeField(base, override any) {
	bv := reflect.ValueOf(base).Elem()
	ov := reflect.ValueOf(override).Elem()
	for i := 0; i < ov.NumField(); i++ {
		f := ov.Field(i)
		if !bv.Field(i).CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
			if !f.IsNil() {
				bv.Field(i).Set(f)
			}
		default:
			if !f.IsZero() {
				bv.Field(i).Set(f)
			}
		}
	}
}
