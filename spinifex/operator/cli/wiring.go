package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// mustBindEnv binds a config key to its environment variables. A wiring error
// would otherwise leave the key silently unbound, so it panics at startup.
func mustBindEnv(key string, envVars ...string) {
	if err := viper.BindEnv(append([]string{key}, envVars...)...); err != nil {
		panic(fmt.Sprintf("bind config key %q to env %v: %v", key, envVars, err)) //nolint:forbidigo // Startup wiring error: fail loudly rather than run with a silently unbound flag or config key.
	}
}

// mustBindPFlag binds a config key to a flag. Looking the flag up on a command
// that does not declare it yields nil, which viper would otherwise drop silently.
func mustBindPFlag(key string, flags *pflag.FlagSet, name string) {
	if err := viper.BindPFlag(key, flags.Lookup(name)); err != nil {
		panic(fmt.Sprintf("bind config key %q to flag %q: %v", key, name, err)) //nolint:forbidigo // Startup wiring error: fail loudly rather than run with a silently unbound flag or config key.
	}
}

// mustMarkFlagRequired marks a flag required, panicking when the command does
// not declare it rather than silently dropping the required check.
func mustMarkFlagRequired(cmd *cobra.Command, name string) {
	if err := cmd.MarkFlagRequired(name); err != nil {
		panic(fmt.Sprintf("mark flag %q required on %q: %v", name, cmd.Name(), err)) //nolint:forbidigo // Startup wiring error: fail loudly rather than run with a silently unbound flag or config key.
	}
}
