// Package cli defines the spx command-line interface: the cobra commands that
// install, configure, start and administer a Spinifex node and its cluster.
package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	cfgFile string
	//lint:ignore U1000 fixing config loading later
	appConfig *config.ClusterConfig
)

// rootCmd represents the base command when called without any subcommands.
var rootCmd = &cobra.Command{
	Use:   "spx",
	Short: "Spinifex - Open source AWS-compatible platform for secure edge deployments",
	Long: `
   _____ _____ _____ _   _ _____ ______ ________   __
  / ____|  __ \_   _| \ | |_   _|  ____|  ____\ \ / /
 | (___ | |__) || | |  \| | | | | |__  | |__   \ V /
  \___ \|  ___/ | | | . ‘ | | | |  __| |  __|   > <
  ____) | |    _| |_| |\  |_| |_| |    | |____ / . \
 |_____/|_|   |_____|_| \_|_____|_|    |______/_/ \_\

Spinifex – Open source AWS-compatible platform for secure edge deployments.
Run EC2, VPC, S3, and EBS-like services on bare metal with full control.
Built for environments where running in the cloud isn’t an option.
Whether you’re deploying to edge sites, private data-centers, or operating
in low-connectivity or highly contested environments
`,
}

// cliLogLevel is the level of the CLI's default logger, retuned by initCLILogger
// once flags are parsed. Service subcommands replace the handler outright via
// initTelemetry, so this governs CLI invocations only.
var cliLogLevel = new(slog.LevelVar)

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() {
	// Claim the process logger before any command runs. Libraries spx links log
	// at Info and above; on a CLI those records are noise that corrupts piped
	// output, so they go to stderr and are dropped below Error by default.
	// Commands report their own failures through fmt.Fprintf(os.Stderr, ...).
	cliLogLevel.Set(slog.LevelError)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cliLogLevel})))

	setOutputStyling(outputStylingEnabled(os.Getenv("NO_COLOR"), os.Getenv("TERM"), isTerminal(os.Stdout)))

	err := rootCmd.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// outputStylingEnabled reports whether spx may emit ANSI colour and text styling:
// only on a terminal that is not TERM=dumb, with NO_COLOR unset or empty.
func outputStylingEnabled(noColor, termName string, stdoutIsTerminal bool) bool {
	return noColor == "" && termName != "dumb" && stdoutIsTerminal
}

// isTerminal reports whether w is a file attached to a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// setOutputStyling switches pterm's colour and bold/underline styling for every
// printer at once. pterm forces colour on at init regardless of the terminal.
func setOutputStyling(enabled bool) {
	if enabled {
		pterm.EnableStyling()
	} else {
		pterm.DisableStyling()
	}
}

// initCLILogger retunes the CLI logger from SPX_LOG_LEVEL, then --verbose so an
// explicit flag wins over the environment. There is no root --debug flag: four
// service subcommands already define one with a service-specific meaning, and a
// root persistent flag of the same name would silently shadow differently
// depending on which command ran. SPX_LOG_LEVEL=debug covers that case.
func initCLILogger() {
	if name := os.Getenv("SPX_LOG_LEVEL"); name != "" {
		var level slog.Level
		if err := level.UnmarshalText([]byte(name)); err == nil {
			cliLogLevel.Set(level)
		} else {
			fmt.Fprintf(os.Stderr, "Warning: ignoring invalid SPX_LOG_LEVEL %q\n", name)
		}
	}
	if verbose, _ := rootCmd.PersistentFlags().GetBool("verbose"); verbose {
		cliLogLevel.Set(slog.LevelInfo)
	}
}

func init() {
	cobra.OnInitialize(initCLILogger, initConfig)

	rootCmd.PersistentFlags().BoolP("verbose", "v", false, "log at info level (default: errors only)")

	// Global flags
	rootCmd.PersistentFlags().String("config", "", "config file (required)")
	mustBindEnv("config", "SPINIFEX_CONFIG_PATH")
	mustBindPFlag("config", rootCmd.PersistentFlags(), "config")

	// Authentication (access_key, secret)
	rootCmd.PersistentFlags().String("access-key", "", "AWS access key (overrides config file and env)")
	mustBindEnv("access-key", "SPINIFEX_ACCESS_KEY")
	mustBindPFlag("access-key", rootCmd.PersistentFlags(), "access-key")

	rootCmd.PersistentFlags().String("secret-key", "", "AWS secret key (overrides config file and env)")
	mustBindEnv("secret-key", "SPINIFEX_SECRET_KEY")
	mustBindPFlag("secret-key", rootCmd.PersistentFlags(), "secret-key")

	rootCmd.PersistentFlags().String("host", "", "AWS Endpoint (overrides config file and env)")
	mustBindEnv("host", "SPINIFEX_HOST")
	mustBindPFlag("host", rootCmd.PersistentFlags(), "host")

	// Viperblock config
	rootCmd.PersistentFlags().String("base-dir", "", "Viperblock base directory (overrides config file and env)")
	mustBindEnv("base-dir", "SPINIFEX_BASE_DIR")
	mustBindPFlag("base-dir", rootCmd.PersistentFlags(), "base-dir")

	// NATS specific flags
	rootCmd.PersistentFlags().String("nats-host", "", "NATS server host (overrides config file and env)")
	mustBindEnv("nats-host", "SPINIFEX_NATS_HOST")
	mustBindPFlag("nats-host", rootCmd.PersistentFlags(), "nats-host")

	rootCmd.PersistentFlags().String("nats-token", "", "NATS authentication token (overrides config file and env)")
	mustBindEnv("nats-token", "SPINIFEX_NATS_TOKEN")
	mustBindPFlag("nats-token", rootCmd.PersistentFlags(), "nats-token")

	rootCmd.PersistentFlags().String("nats-subject", "", "NATS subscription subject (overrides config file and env)")
	mustBindEnv("nats-subject", "SPINIFEX_NATS_SUBJECT")
	mustBindPFlag("nats-subject", rootCmd.PersistentFlags(), "nats-subject")

	// Bind flags to viper
	//viper.BindPFlag("nats.host", rootCmd.PersistentFlags().Lookup("nats-host"))
	//viper.BindPFlag("nats.acl.token", rootCmd.PersistentFlags().Lookup("nats-token"))
	//viper.BindPFlag("nats.sub.subject", rootCmd.PersistentFlags().Lookup("nats-subject"))
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	var err error

	// Load configuration
	appConfig, err = config.LoadConfig(cfgFile)

	if err != nil {
		// If a config file was explicitly provided, treat load failure as fatal
		if cfgFile != "" {
			fmt.Fprintf(os.Stderr, "Error: failed to load config %s: %v\n", cfgFile, err)
			os.Exit(1)
		}
		// No config specified — continue with env/defaults (e.g., spx --help)
	}
}
