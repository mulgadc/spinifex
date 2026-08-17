package handlers_rds

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
)

// The engine-neutral half of the parameter catalog: the spec type, its parsing
// and range checks, and the resolver. Each engine supplies its own table, its
// size-derived formulas and its combination checks; everything here is shared.

// When a parameter takes effect. Static settings are stored and reported
// pending-reboot; dynamic ones are adopted by a reload.
const (
	ApplyTypeStatic  = "static"
	ApplyTypeDynamic = "dynamic"
)

// AWS's own ApplyMethod values, echoed back on DescribeDBParameters.
const (
	ApplyMethodImmediate     = "immediate"
	ApplyMethodPendingReboot = "pending-reboot"
)

// Where a reported value came from. AWS distinguishes these and the Terraform
// provider reads them, so a computed default must not be reported as user.
const (
	ParameterSourceUser          = "user"
	ParameterSourceEngineDefault = "engine-default"
)

// The parameter data types the catalog offers. Every one is validated on input,
// so a value the API accepts is one the engine will parse.
const (
	ParamTypeInteger = "integer"
	ParamTypeReal    = "real"
	ParamTypeBoolean = "boolean"
	ParamTypeString  = "string"
	ParamTypeEnum    = "enum"
)

// One catalog entry. Exactly one of Default and DefaultFor is set: a literal for
// the parameters whose engine default is size-independent, and a formula over
// the instance class's memory for the ones that are not.
type ParameterSpec struct {
	Name        string
	DataType    string
	ApplyType   string
	Description string
	// False for a setting AWS exposes but this platform pins, so the refusal names
	// it and reads as policy. One AWS owns too is absent from the catalog instead,
	// which reads as the engine not offering it.
	IsModifiable bool

	Default string
	// Evaluated against the class's memory. The result is a literal, so the
	// customer-facing API never sees a formula.
	DefaultFor func(memoryMiB int64) string
	// The generous class-specific ceiling for a size-derived parameter. It
	// prevents a large-class literal from making a smaller guest unbootable.
	MaxFor func(memoryMiB int64) int64

	// Inclusive bounds for integer and real parameters. Both zero means
	// unbounded below and above.
	Min, Max float64
	// The permitted values of an enum parameter, lowest-to-highest where the
	// engine gives them an order.
	Enum []string
	// The engine's own unit suffix, reported in AllowedValues so a customer can
	// see what an integer means. Empty for unitless settings.
	Unit string

	// The engine's own rule for a value the generic type and range checks cannot
	// express, such as a zone name that must resolve or a comma-separated list of
	// engine mode names. Runs last, on the trimmed value.
	Validate func(value string) error

	// The spelling the engine accepts for this setting in its option file, when
	// that differs from the name the customer sets. Empty means the two are the
	// same, which is every parameter but MariaDB's time_zone.
	optionFileName string
}

// Indexes the specs by name and fails the build-equivalent — process start — on
// a malformed entry, so a catalog typo cannot reach a customer's create.
func buildParameterCatalog(specs ...ParameterSpec) map[string]ParameterSpec {
	out := make(map[string]ParameterSpec, len(specs))
	for _, spec := range specs {
		switch {
		case spec.Name == "":
			panic("rds: parameter catalog holds an unnamed entry")
		case spec.Default == "" && spec.DefaultFor == nil:
			panic("rds: parameter catalog entry " + spec.Name + " has no default")
		case spec.Default != "" && spec.DefaultFor != nil:
			panic("rds: parameter catalog entry " + spec.Name + " has both a literal and a computed default")
		case (spec.DefaultFor == nil) != (spec.MaxFor == nil):
			panic("rds: parameter catalog entry " + spec.Name + " must pair its computed default and class ceiling")
		}
		if _, exists := out[spec.Name]; exists {
			panic("rds: parameter catalog entry " + spec.Name + " is duplicated")
		}
		out[spec.Name] = spec
	}
	return out
}

// The catalog entry for a parameter name, or false when the engine has no such
// setting or it is one this platform does not expose.
func (e Engine) LookupParameter(name string) (ParameterSpec, bool) {
	spec, ok := e.catalog[strings.ToLower(strings.TrimSpace(name))]
	return spec, ok
}

// The spelling to write into the engine's option file for a parameter the
// customer set. A name the server does not accept there is a boot loop with the
// bad file already on the data volume, so the two names are kept apart.
func (e Engine) OptionFileName(name string) string {
	spec, ok := e.LookupParameter(name)
	if !ok || spec.optionFileName == "" {
		return name
	}
	return spec.optionFileName
}

// The parameter that requires TLS of a client connection, under AWS's own name
// for it. Exported for the in-guest agent, which derives enforcement from the
// installed set and has no business knowing which engine it is running.
func (e Engine) TLSEnforcementParameter() string {
	return e.tlsEnforcementParameter
}

// Sorted, so a describe returns the same order on every call and Terraform does
// not read a reshuffle as drift.
func (e Engine) CatalogParameterNames() []string {
	return slices.Sorted(maps.Keys(e.catalog))
}

// The engine default for one parameter at one instance class: the literal, or
// the formula evaluated against the class's memory.
func (s ParameterSpec) DefaultAt(memoryMiB int64) string {
	if s.DefaultFor != nil {
		return s.DefaultFor(memoryMiB)
	}
	return s.Default
}

// The AllowedValues string AWS reports: a range for numerics, the alternatives
// for an enum or boolean. Empty for a free-form string, as AWS leaves it.
func (s ParameterSpec) AllowedValues() string {
	switch s.DataType {
	case ParamTypeInteger, ParamTypeReal:
		bounds := fmt.Sprintf("%s-%s", formatBound(s.Min), formatBound(s.Max))
		if s.Unit != "" {
			return bounds + " (" + s.Unit + ")"
		}
		return bounds
	case ParamTypeEnum:
		return strings.Join(s.Enum, ",")
	case ParamTypeBoolean:
		return strings.Join(booleanSpellings, ",")
	default:
		return ""
	}
}

// The spellings the API accepts for a boolean, on every engine. MariaDB's own
// parser refuses yes and no, which costs nothing here: canonicalBoolean turns
// all eight into 1 or 0 before a value reaches either guest.
var booleanSpellings = []string{"on", "off", "true", "false", "yes", "no", "1", "0"}

// The one spelling a boolean reaches the guest as. Both engines parse 1 and 0,
// neither parses all eight the API accepts, and a guest deriving behaviour from
// a value has one literal to compare rather than a vocabulary.
//
// The set is closed: an override reaches this only after it was validated
// against the spellings above, so anything else is a catalog default that would
// not have passed the same check.
func canonicalBoolean(name, value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on", "true", "yes", "1":
		return "1", nil
	case "off", "false", "no", "0":
		return "0", nil
	}
	return "", awserrors.Errorf(awserrors.ErrorServerInternal,
		"the catalog default %q of parameter %s is not a boolean", value, name)
}

// Integral bounds print without a decimal point, so an integer parameter's range
// does not read as a real one's.
func formatBound(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// The memory an instance class's guest has, which every size-derived default is
// computed from. An unknown class is a validation failure upstream, so this is
// the last line rather than the check.
func classMemoryMiB(instanceClass string) (int64, error) {
	instanceType, err := InstanceTypeForClass(instanceClass)
	if err != nil {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DBInstanceClass %q is not supported; supported classes are %s", instanceClass, strings.Join(SupportedInstanceClasses(), ", "))
	}
	memoryMiB, ok := instancetypes.DefaultMemoryMiB(instanceType)
	if !ok || memoryMiB <= 0 {
		return 0, awserrors.Errorf(awserrors.ErrorServerInternal,
			"no memory footprint is known for instance type %s", instanceType)
	}
	return memoryMiB, nil
}

// AWS accepts formulas like {DBInstanceClassMemory/32768} and references like
// DBInstanceClassMemory from a customer. This platform does not: the catalog
// validates literals, and a formula that reached the engine unvalidated would be
// a startup failure rather than an API error.
func rejectFormulaValue(name, value string) error {
	if !strings.ContainsAny(value, "{}") && !strings.Contains(value, "DBInstanceClass") {
		return nil
	}
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"the value %q of parameter %s is a formula; only literal values are accepted, "+
			"and the size-derived defaults are computed for you", value, name)
}

// Checks one customer-supplied value against its catalog entry. A parameter the
// catalog does not hold, one the platform owns, and one outside its own range
// are all rejected here — at the API, rather than by an engine that then refuses
// to start with the bad config already on the data volume.
func (e Engine) validateParameterValue(name, value string) (ParameterSpec, error) {
	spec, ok := e.LookupParameter(name)
	if !ok {
		return ParameterSpec{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%q is not a parameter this engine exposes", name)
	}
	if !spec.IsModifiable {
		return ParameterSpec{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"parameter %s is not modifiable", spec.Name)
	}
	if err := spec.validateValue(value); err != nil {
		return ParameterSpec{}, err
	}
	return spec, nil
}

// The type, range and engine-specific checks for one value, without the
// modifiability gate. A pinned entry's own default goes through these too: it is
// still a literal the engine has to parse.
func (s ParameterSpec) validateValue(value string) error {
	if err := rejectFormulaValue(s.Name, value); err != nil {
		return err
	}

	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"parameter %s was given an empty value", s.Name)
	}
	switch s.DataType {
	case ParamTypeInteger:
		n, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return typeError(s, value, "an integer")
		}
		if float64(n) < s.Min || float64(n) > s.Max {
			return rangeError(s, value)
		}
	case ParamTypeReal:
		f, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return typeError(s, value, "a number")
		}
		if f < s.Min || f > s.Max {
			return rangeError(s, value)
		}
	case ParamTypeBoolean:
		if !slices.Contains(booleanSpellings, strings.ToLower(trimmed)) {
			return typeError(s, value,
				"a boolean ("+strings.Join(booleanSpellings, ", ")+")")
		}
	case ParamTypeEnum:
		if !slices.Contains(s.Enum, strings.ToLower(trimmed)) {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"parameter %s does not accept %q; allowed values are %s", s.Name, value, strings.Join(s.Enum, ", "))
		}
	case ParamTypeString:
		if err := validateStringParameter(s, trimmed); err != nil {
			return err
		}
	}
	if s.Validate != nil {
		return s.Validate(trimmed)
	}
	return nil
}

const maxStringParameterBytes = 1024

// The bounds every engine's free-form string shares. What a particular setting
// means is the engine's own rule, carried on the spec's Validate.
func validateStringParameter(spec ParameterSpec, trimmed string) error {
	if len(trimmed) > maxStringParameterBytes {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"parameter %s exceeds the maximum length of %d bytes", spec.Name, maxStringParameterBytes)
	}
	if strings.HasSuffix(trimmed, `\`) {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"parameter %s cannot end with a backslash", spec.Name)
	}
	if strings.IndexFunc(trimmed, unicode.IsControl) >= 0 {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"parameter %s cannot contain control characters", spec.Name)
	}
	return nil
}

func typeError(spec ParameterSpec, value, want string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"parameter %s takes %s, not %q", spec.Name, want, value)
}

func rangeError(spec ParameterSpec, value string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"the value %q of parameter %s is outside its allowed range %s", value, spec.Name, spec.AllowedValues())
}

// The full parameter set an instance runs with: every catalog default evaluated
// at the instance's class, overlaid with the group's stored overrides. The
// result is literals only, sorted by name so a re-resolve that changed nothing
// produces a byte-identical include, and every boolean canonicalised.
//
// Overrides are re-validated rather than trusted: a catalog whose bounds
// tightened must not keep handing the engine a value it would now reject.
func (e Engine) ResolveEffectiveParameters(instanceClass string, overrides map[string]string) ([]Parameter, error) {
	if e.validateCombinations == nil {
		return nil, awserrors.Errorf(awserrors.ErrorServerInternal,
			"engine %s registers no parameter combination checks", e.Name)
	}
	memoryMiB, err := classMemoryMiB(instanceClass)
	if err != nil {
		return nil, err
	}
	names := e.CatalogParameterNames()
	resolved := make([]Parameter, 0, len(names))
	for _, name := range names {
		spec := e.catalog[name]
		value := spec.DefaultAt(memoryMiB)
		if override, ok := overrides[name]; ok {
			if _, err := e.validateParameterValue(name, override); err != nil {
				return nil, err
			}
			if err := validateClassParameterValue(instanceClass, memoryMiB, spec, override); err != nil {
				return nil, err
			}
			value = override
		}
		if spec.DataType == ParamTypeBoolean {
			canonical, err := canonicalBoolean(name, value)
			if err != nil {
				return nil, err
			}
			value = canonical
		}
		resolved = append(resolved, Parameter{Name: name, Value: value})
	}
	if err := e.validateCombinations(resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

// The bytes of class memory an RDS parameter formula divides. Real RDS exposes
// it as {DBInstanceClassMemory}, and the resolver evaluates the formulas here
// rather than passing them to the engine.
const mibToBytes = 1024 * 1024

func clampInt64(v, lo, hi int64) int64 {
	return min(max(v, lo), hi)
}

// Reads one setting out of a resolved set for a combination check. The set is
// every catalog name by construction, so an absent one is a catalog bug rather
// than anything a customer did.
func resolvedValues(params []Parameter) map[string]string {
	values := make(map[string]string, len(params))
	for _, param := range params {
		values[param.Name] = strings.ToLower(strings.TrimSpace(param.Value))
	}
	return values
}

func resolvedString(values map[string]string, name string) (string, error) {
	value, ok := values[name]
	if !ok {
		return "", awserrors.Errorf(awserrors.ErrorServerInternal,
			"resolved parameter set is missing %s", name)
	}
	return value, nil
}

func resolvedInteger(values map[string]string, name string) (int64, error) {
	value, err := resolvedString(values, name)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, awserrors.Errorf(awserrors.ErrorServerInternal,
			"resolved parameter %s has invalid integer value %q", name, value)
	}
	return n, nil
}

func validateClassParameterValue(instanceClass string, memoryMiB int64, spec ParameterSpec, value string) error {
	if spec.MaxFor == nil {
		return nil
	}
	requested, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return typeError(spec, value, "an integer")
	}
	ceiling := spec.MaxFor(memoryMiB)
	if requested > ceiling {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"the value %q of parameter %s is too large for DB instance class %s; the class ceiling is %d",
			value, spec.Name, instanceClass, ceiling)
	}
	return nil
}
