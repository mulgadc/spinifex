package cmd

import (
	"fmt"
	"os"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
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

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	// Global flags
	rootCmd.PersistentFlags().String("config", "", "config file (required)")
	viper.BindEnv("config", "SPINIFEX_CONFIG_PATH")
	viper.BindPFlag("config", rootCmd.PersistentFlags().Lookup("config"))

	// Authentication (access_key, secret)
	rootCmd.PersistentFlags().String("access-key", "", "AWS access key (overrides config file and env)")
	viper.BindEnv("access-key", "SPINIFEX_ACCESS_KEY")
	viper.BindPFlag("access-key", rootCmd.PersistentFlags().Lookup("access-key"))

	rootCmd.PersistentFlags().String("secret-key", "", "AWS secret key (overrides config file and env)")
	viper.BindEnv("secret-key", "SPINIFEX_SECRET_KEY")
	viper.BindPFlag("secret-key", rootCmd.PersistentFlags().Lookup("secret-key"))

	rootCmd.PersistentFlags().String("host", "", "AWS Endpoint (overrides config file and env)")
	viper.BindEnv("host", "SPINIFEX_HOST")
	viper.BindPFlag("host", rootCmd.PersistentFlags().Lookup("host"))

	// Viperblock config
	rootCmd.PersistentFlags().String("base-dir", "", "Viperblock base directory (overrides config file and env)")
	viper.BindEnv("base-dir", "SPINIFEX_BASE_DIR")
	viper.BindPFlag("base-dir", rootCmd.PersistentFlags().Lookup("base-dir"))

	// NATS specific flags
	rootCmd.PersistentFlags().String("nats-host", "", "NATS server host (overrides config file and env)")
	viper.BindEnv("nats-host", "SPINIFEX_NATS_HOST")
	viper.BindPFlag("nats-host", rootCmd.PersistentFlags().Lookup("nats-host"))

	rootCmd.PersistentFlags().String("nats-token", "", "NATS authentication token (overrides config file and env)")
	viper.BindEnv("nats-token", "SPINIFEX_NATS_TOKEN")
	viper.BindPFlag("nats-token", rootCmd.PersistentFlags().Lookup("nats-token"))

	rootCmd.PersistentFlags().String("nats-subject", "", "NATS subscription subject (overrides config file and env)")
	viper.BindEnv("nats-subject", "SPINIFEX_NATS_SUBJECT")
	viper.BindPFlag("nats-subject", rootCmd.PersistentFlags().Lookup("nats-subject"))

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
