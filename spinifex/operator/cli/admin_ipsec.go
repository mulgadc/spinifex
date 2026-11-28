package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

// resolveInitIPSec returns the IPsec setting init writes. An explicit --ipsec
// wins. Without one, a re-init keeps what the existing config holds, so
// --force never changes a cluster's encryption, and a new install takes the default.
func resolveInitIPSec(flagValue, flagChanged bool, tomlPath string) (bool, error) {
	if flagChanged {
		return flagValue, nil
	}
	raw, err := os.ReadFile(tomlPath)
	if errors.Is(err, os.ErrNotExist) {
		return flagValue, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the existing config %s, whose IPsec setting a re-init keeps: %w", tomlPath, err)
	}
	var cfg struct {
		Network struct {
			IPSecEnabled *bool `toml:"ipsec_enabled"`
		} `toml:"network"`
	}
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return false, fmt.Errorf("parse the existing config %s, whose IPsec setting a re-init keeps: %w", tomlPath, err)
	}
	// A config with no key predates it and reads as encrypted, as the daemon does.
	if cfg.Network.IPSecEnabled == nil {
		return true, nil
	}
	return *cfg.Network.IPSecEnabled, nil
}
