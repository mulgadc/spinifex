package handlers_rds

import (
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// The control-plane half of the engine seam: what CreateDBInstance needs to
// validate a request and assemble a bootstrap config. The in-guest half —
// initdb, quiesce, live password apply — lives in rds-init and rds-agent.
type Engine struct {
	Name string
	// Pinned for v1. A request naming another version is rejected rather than
	// served by an image that is not the one asked for.
	MajorVersion string
	DefaultPort  int64
	// Identifiers the engine reserves for itself, which a master role may not
	// take. Matched case-insensitively.
	reservedUsernames []string
	// Prefixes the engine reserves for internal roles.
	reservedUsernamePrefixes []string
	// The engine's own identifier limit for a role name. The character rule is
	// shared across engines; the length is not.
	maxUsernameLen int

	// The engine's parameter table, keyed by parameter name. The generic spec
	// machinery is shared; only the table and its formulas are per-engine.
	catalog map[string]ParameterSpec
	// Cross-parameter checks a resolved set must satisfy, which are the
	// combinations the engine itself would refuse to start under.
	validateCombinations func([]Parameter) error
}

var engines = map[string]Engine{
	enginePostgres.Name: enginePostgres,
}

// The same registry keyed by parameter-group family, for the callers that hold
// only a family string and have no instance to derive an engine from. Family
// and engine are 1:1 by construction, so one registry serves both.
var enginesByFamily = indexEnginesByFamily()

func indexEnginesByFamily() map[string]Engine {
	out := make(map[string]Engine, len(engines))
	for _, engine := range engines {
		family := engine.ParameterGroupFamily()
		if _, exists := out[family]; exists {
			panic("rds: parameter group family " + family + " is claimed by two engines")
		}
		out[family] = engine
	}
	return out
}

// The pinned version an AMI lookup resolves against, which is the major alone:
// the AMI carries the major, and a minor is chosen by the image build.
func (e Engine) EngineVersion() string {
	return e.MajorVersion
}

// The parameter-group name AWS clients expect when none is named. The group is
// implicit: it is resolvable and reportable without ever having been created,
// and is neither modifiable nor deletable.
func (e Engine) DefaultParameterGroupName() string {
	return defaultParameterGroupPrefix + e.ParameterGroupFamily()
}

// The family every parameter group of this engine belongs to. v1 pins one major
// per engine, so a family is a name rather than a version axis.
func (e Engine) ParameterGroupFamily() string {
	return e.Name + e.MajorVersion
}

// An unknown engine is rejected at validation, before any volume or ENI exists.
func LookupEngine(name string) (Engine, error) {
	engine, ok := engines[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Engine{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"engine %q is not supported; supported engines are %s", name, strings.Join(SupportedEngines(), ", "))
	}
	return engine, nil
}

func SupportedEngines() []string {
	return slices.Sorted(maps.Keys(engines))
}

// An empty version takes the pin. A supplied one must name the pinned major
// exactly, since the image does not promise any particular minor version.
func (e Engine) ValidateVersion(version string) error {
	if version == "" || version == e.MajorVersion {
		return nil
	}
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"EngineVersion %q is not available; %s %s is the only supported version", version, e.Name, e.MajorVersion)
}

// Mirrors the engine's own rules rather than a generic identifier check, so a
// name the control plane accepts cannot fail at initdb time inside the guest.
func (e Engine) ValidateMasterUsername(username string) error {
	if username == "" {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "MasterUsername is required")
	}
	if len(username) > e.maxUsernameLen {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"MasterUsername must be at most %d characters", e.maxUsernameLen)
	}
	if !isLetter(rune(username[0])) {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "MasterUsername must begin with a letter")
	}
	for _, r := range username {
		if !isLetter(r) && !isDigit(r) && r != '_' {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"MasterUsername may contain only letters, digits and underscores")
		}
	}
	return e.ValidateUsernameNotReserved(username)
}

// The reserved-role half of the check on its own, exported for the in-guest
// agent: its live password apply runs as the cluster superuser, so it re-checks
// the name it is handed rather than trusting the control plane to have done it.
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

// Bounds and the printable-ASCII range AWS accepts. The password is never
// inspected beyond this and never stored in cleartext past the first bootstrap
// fetch.
func ValidateMasterUserPassword(password string) error {
	switch {
	case password == "":
		return errors.New(awserrors.ErrorInvalidParameterValue + ": MasterUserPassword is required")
	case len(password) < minMasterPasswordLen || len(password) > maxMasterPasswordLen:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"MasterUserPassword must be between %d and %d characters", minMasterPasswordLen, maxMasterPasswordLen)
	}
	for _, r := range password {
		// A control character would also survive the bootstrap handoff and defeat
		// the line-oriented redaction that keeps the password off the guest
		// console, so the range is refused here rather than sanitised there.
		if r < 0x20 || r > 0x7e {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"MasterUserPassword may only contain printable ASCII characters")
		}
		if r == '/' || r == '"' || r == '@' || r == ' ' {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"MasterUserPassword may not contain '/', '\"', '@' or spaces")
		}
	}
	return nil
}

const (
	minMasterPasswordLen = 8
	maxMasterPasswordLen = 128
)

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}
