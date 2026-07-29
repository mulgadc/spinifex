package handlers_rds

import (
	"errors"
	"fmt"
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
}

var engines = map[string]Engine{
	enginePostgres.Name: enginePostgres,
}

// The pinned version an AMI lookup resolves against, which is the major alone:
// the AMI carries the major, and a minor is chosen by the image build.
func (e Engine) EngineVersion() string {
	return e.MajorVersion
}

// The parameter-group name AWS clients expect when none is named. rds-7
// materialises the group itself; here it is only the name a request may carry.
func (e Engine) DefaultParameterGroupName() string {
	return "default." + e.Name + e.MajorVersion
}

// An unknown engine is rejected at validation, before any volume or ENI exists.
func LookupEngine(name string) (Engine, error) {
	engine, ok := engines[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Engine{}, fmt.Errorf("%s: engine %q is not supported; supported engines are %s",
			awserrors.ErrorInvalidParameterValue, name, strings.Join(SupportedEngines(), ", "))
	}
	return engine, nil
}

func SupportedEngines() []string {
	return slices.Sorted(maps.Keys(engines))
}

// An empty version takes the pin. A supplied one must name the pinned major,
// with or without a minor, since a mismatch would silently run a version the
// customer did not ask for (D19).
func (e Engine) ValidateVersion(version string) error {
	v := strings.TrimSpace(version)
	if v == "" || v == e.MajorVersion {
		return nil
	}
	if major, _, ok := strings.Cut(v, "."); ok && major == e.MajorVersion {
		return nil
	}
	return fmt.Errorf("%s: EngineVersion %q is not available; %s %s is the only supported version",
		awserrors.ErrorInvalidParameterValue, version, e.Name, e.MajorVersion)
}

// Mirrors the engine's own rules rather than a generic identifier check, so a
// name the control plane accepts cannot fail at initdb time inside the guest.
func (e Engine) ValidateMasterUsername(username string) error {
	if username == "" {
		return fmt.Errorf("%s: MasterUsername is required", awserrors.ErrorInvalidParameterValue)
	}
	if len(username) > maxMasterUsernameLen {
		return fmt.Errorf("%s: MasterUsername must be at most %d characters",
			awserrors.ErrorInvalidParameterValue, maxMasterUsernameLen)
	}
	if !isLetter(rune(username[0])) {
		return fmt.Errorf("%s: MasterUsername must begin with a letter", awserrors.ErrorInvalidParameterValue)
	}
	for _, r := range username {
		if !isLetter(r) && !isDigit(r) && r != '_' {
			return fmt.Errorf("%s: MasterUsername may contain only letters, digits and underscores",
				awserrors.ErrorInvalidParameterValue)
		}
	}
	lower := strings.ToLower(username)
	if slices.Contains(e.reservedUsernames, lower) {
		return fmt.Errorf("%s: MasterUsername %q is reserved by %s",
			awserrors.ErrorInvalidParameterValue, username, e.Name)
	}
	for _, prefix := range e.reservedUsernamePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return fmt.Errorf("%s: MasterUsername may not begin with %q, which %s reserves",
				awserrors.ErrorInvalidParameterValue, prefix, e.Name)
		}
	}
	return nil
}

// Bounds only. The password is never inspected beyond this and never stored in
// cleartext past the first bootstrap fetch (D8).
func ValidateMasterUserPassword(password string) error {
	switch {
	case password == "":
		return errors.New(awserrors.ErrorInvalidParameterValue + ": MasterUserPassword is required")
	case len(password) < minMasterPasswordLen || len(password) > maxMasterPasswordLen:
		return fmt.Errorf("%s: MasterUserPassword must be between %d and %d characters",
			awserrors.ErrorInvalidParameterValue, minMasterPasswordLen, maxMasterPasswordLen)
	}
	for _, r := range password {
		if r == '/' || r == '"' || r == '@' || r == ' ' {
			return fmt.Errorf("%s: MasterUserPassword may not contain '/', '\"', '@' or spaces",
				awserrors.ErrorInvalidParameterValue)
		}
	}
	return nil
}

const (
	maxMasterUsernameLen = 63
	minMasterPasswordLen = 8
	maxMasterPasswordLen = 128
)

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}
