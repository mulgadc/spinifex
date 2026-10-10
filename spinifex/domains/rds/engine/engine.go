// Package engine owns the RDS engine catalogue: the registry of supported
// engines, their parameter catalogues, and the size-derived sizing table a
// parameter default is evaluated against. It is the control-plane half of
// the engine seam; the in-guest half lives in rds-init and rds-agent.
package engine

import (
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// Setting is one resolved name/value pair. Separate from any caller's own
// parameter type, so this package reports a result without depending on how
// a caller stores or renders it.
type Setting struct {
	Name  string
	Value string
}

// Engine is the control-plane half of the engine seam: what CreateDBInstance needs to validate a
// request and assemble a bootstrap config. The in-guest half — initdb, quiesce, live password apply —
// lives in rds-init and rds-agent.
type Engine struct {
	Name string
	// Pinned for v1. A request naming another version is rejected rather than
	// served by an image that is not the one asked for.
	MajorVersion string
	DefaultPort  int64
	// How the engine names itself in a catalog listing, which is not its API
	// identifier: a console renders this rather than "postgres".
	description string
	// The AWS licence model the engine is offered under, which is a property of
	// the engine's own licence rather than of this platform.
	licenseModel string
	// Identifiers the engine reserves for itself, which a master role may not
	// take. Matched case-insensitively.
	reservedUsernames []string
	// Prefixes the engine reserves for internal roles.
	reservedUsernamePrefixes []string
	// The engine's own identifier limit for a role name. The character rule is
	// shared across engines; the length is not.
	maxUsernameLen int
	// The engine's rule for an initial database name, which the guest
	// interpolates into a CREATE DATABASE.
	validateDBName func(string) error

	// The engine's parameter table, keyed by parameter name. The generic spec
	// machinery is shared; only the table and its formulas are per-engine.
	catalog map[string]ParameterSpec
	// Cross-parameter checks a resolved set must satisfy, which are the
	// combinations the engine itself would refuse to start under.
	validateCombinations func([]Setting) error
	// The engine's own name for the setting that requires TLS of a client
	// connection, which is AWS's name for it. Named here so the control plane and
	// the guest agree on the key without either spelling out an engine.
	tlsEnforcementParameter string

	// What a snapshot taken without a quiesce actually recovers on restore, which
	// is the engine's own guarantee and not a shared one.
	crashRecoveryNote string
	// The same guarantee stated for the next start rather than for a restore,
	// which is what an engine that would not shut down cleanly gets.
	uncleanStopNote string
}

var engines = map[string]Engine{
	enginePostgres.Name: enginePostgres,
	engineMariaDB.Name:  engineMariaDB,
}

// The same registry keyed by parameter-group family, for the callers that hold
// only a family string and have no instance to derive an engine from. Family
// and engine are 1:1 by construction, so one registry serves both.
var enginesByFamily, engineRegistryValidationErr = indexEnginesByFamily(engines)

// The prefix AWS reserves for the groups the service itself owns. Duplicated
// against handlers_rds's own copy deliberately: each package's constant backs
// a different check (this one a name this package derives, that one a name a
// caller recognises), and neither may reach across the boundary to the other.
const defaultParameterGroupPrefix = "default."

// ValidateEngineRegistry reports invalid built-in engine metadata before RDS starts.
func ValidateEngineRegistry() error {
	if err := validateParameterCatalogs(); err != nil {
		return err
	}
	return engineRegistryValidationErr
}

func indexEnginesByFamily(registry map[string]Engine) (map[string]Engine, error) {
	out := make(map[string]Engine, len(registry))
	for _, engine := range registry {
		if engine.validateDBName == nil {
			return nil, errors.New("rds: engine " + engine.Name + " registers no DBName rule")
		}
		// Without these, snapshot and unclean-stop events cannot explain what the
		// engine will recover.
		if engine.crashRecoveryNote == "" {
			return nil, errors.New("rds: engine " + engine.Name + " registers no crash-recovery note")
		}
		if engine.uncleanStopNote == "" {
			return nil, errors.New("rds: engine " + engine.Name + " registers no unclean-stop note")
		}
		// A name no catalog entry answers to would leave the guest deriving
		// enforcement from a key nothing ever writes, which reads as not enforcing.
		if name := engine.tlsEnforcementParameter; name != "" {
			spec, ok := engine.catalog[name]
			if !ok || spec.DataType != ParamTypeBoolean {
				return nil, errors.New("rds: engine " + engine.Name + " names " + name + ", which is not a boolean parameter it exposes")
			}
		}
		family := engine.ParameterGroupFamily()
		if _, exists := out[family]; exists {
			return nil, errors.New("rds: parameter group family " + family + " is claimed by two engines")
		}
		out[family] = engine
	}
	return out, nil
}

// EngineVersion returns the pinned version an AMI lookup resolves against, which is the major alone:
// the AMI carries the major, and a minor is chosen by the image build.
func (e Engine) EngineVersion() string {
	return e.MajorVersion
}

// Description returns the engine's own name for itself, as a describe reports it.
func (e Engine) Description() string {
	return e.description
}

// LicenseModel returns the AWS license model name, which an orderable option carries and which a
// client may filter on.
func (e Engine) LicenseModel() string {
	return e.licenseModel
}

// DefaultParameterGroupName returns the parameter-group name AWS clients expect when none is named. The
// group is implicit: it is resolvable and reportable without ever having been created, and is neither
// modifiable nor deletable.
func (e Engine) DefaultParameterGroupName() string {
	return defaultParameterGroupPrefix + e.ParameterGroupFamily()
}

// ParameterGroupFamily returns the family every parameter group of this engine belongs to. v1 pins one
// major per engine, so a family is a name rather than a version axis.
func (e Engine) ParameterGroupFamily() string {
	return e.Name + e.MajorVersion
}

// CrashRecoveryNote returns what a snapshot taken without a quiesce actually
// recovers on restore, for the event the control plane records alongside it.
func (e Engine) CrashRecoveryNote() string {
	return e.crashRecoveryNote
}

// UncleanStopNote returns the same guarantee stated for the next start rather
// than for a restore, for the event an unclean stop records.
func (e Engine) UncleanStopNote() string {
	return e.uncleanStopNote
}

// LookupEngine resolves an engine name case-insensitively. An unknown engine is rejected with
// InvalidParameterValue at validation, before any volume or ENI exists.
func LookupEngine(name string) (Engine, error) {
	engine, ok := engines[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Engine{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"engine %q is not supported; supported engines are %s", name, strings.Join(SupportedEngines(), ", "))
	}
	return engine, nil
}

// SupportedEngines returns the engine names CreateDBInstance accepts, sorted for stable error messages.
func SupportedEngines() []string {
	return slices.Sorted(maps.Keys(engines))
}

// EngineForFamily returns the engine a parameter group's family belongs to, for the paths that hold a
// group and no instance. A family naming no engine is a group written by a build that offered an
// engine this one does not.
func EngineForFamily(family string) (Engine, error) {
	engine, ok := enginesByFamily[normaliseFamily(family)]
	if !ok {
		return Engine{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"ParameterGroupFamily %s is not a valid parameter group family. Supported families: %s.",
			family, strings.Join(SupportedParameterGroupFamilies(), ", "))
	}
	return engine, nil
}

// EngineForDefaultParameterGroup returns the engine whose implicit default group carries this name, so
// an unrecognised default.* name is a not-found rather than a group that resolves to nothing.
func EngineForDefaultParameterGroup(name string) (Engine, bool) {
	for _, engine := range engines {
		if engine.DefaultParameterGroupName() == name {
			return engine, true
		}
	}
	return Engine{}, false
}

// SupportedParameterGroupFamilies returns every engine's parameter group family name (e.g. postgres18),
// sorted.
func SupportedParameterGroupFamilies() []string {
	return slices.Sorted(maps.Keys(enginesByFamily))
}

func normaliseFamily(family string) string {
	return strings.ToLower(strings.TrimSpace(family))
}

// ValidateVersion accepts an empty version, which takes the pin. A supplied one must name the pinned
// major exactly, since the image does not promise any particular minor version.
func (e Engine) ValidateVersion(version string) error {
	if version == "" || version == e.MajorVersion {
		return nil
	}
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"EngineVersion %q is not available; %s %s is the only supported version", version, e.Name, e.MajorVersion)
}

// ValidateMasterUsername mirrors the engine's own rules rather than a generic identifier check, so a
// name the control plane accepts cannot fail at initdb time inside the guest.
func (e Engine) ValidateMasterUsername(username string) error {
	if err := validateIdentifier("MasterUsername", username, e.maxUsernameLen, false); err != nil {
		return err
	}
	return e.ValidateUsernameNotReserved(username)
}

// ValidateUsernameNotReserved is the reserved-role half of the check on its own, exported for the
// in-guest agent: its live password apply runs as the cluster superuser, so it re-checks the name it
// is handed rather than trusting the control plane to have done it.
func (e Engine) ValidateUsernameNotReserved(username string) error {
	lower := strings.ToLower(strings.TrimSpace(username))
	if slices.Contains(e.reservedUsernames, lower) {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"MasterUsername %q is reserved by %s", username, e.Name)
	}
	for _, prefix := range e.reservedUsernamePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"MasterUsername may not begin with %q, which %s reserves", prefix, e.Name)
		}
	}
	return nil
}

// ValidateDBName checks the initial database name, which AWS leaves optional: an empty name creates no
// database at all rather than one named by default.
func (e Engine) ValidateDBName(name string) error {
	return e.validateDBName(name)
}

// The shared rule, taking each engine's own identifier limit. The character set
// is narrower than either engine accepts, because it is also what makes the name
// safe to interpolate into the CREATE DATABASE rds-init builds inside the guest.
func dbNameRule(maxLen int) func(string) error {
	return func(name string) error {
		return validateIdentifier("DBName", name, maxLen, true)
	}
}

// The character rule both identifiers share, stated once. Only the length limit
// and whether an empty value is legal differ between them, so field names the
// parameter each message is about.
func validateIdentifier(field, value string, maxLen int, allowEmpty bool) error {
	switch {
	case value == "":
		if allowEmpty {
			return nil
		}
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s is required", field)
	case len(value) > maxLen:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s must be at most %d characters", field, maxLen)
	case !isLetter(rune(value[0])):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s must begin with a letter", field)
	}
	for _, r := range value {
		if !isLetter(r) && !isDigit(r) && r != '_' {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"%s may contain only letters, digits and underscores", field)
		}
	}
	return nil
}

// isLetter and isDigit are duplicated against handlers_rds's own copy
// deliberately: that one backs identifier checks this package does not own,
// and neither may reach across the boundary to the other.
func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}
