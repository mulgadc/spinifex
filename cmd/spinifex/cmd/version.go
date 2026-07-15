package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Version variables set via ldflags at build time.
// Example: go build -ldflags "-X github.com/mulgadc/spinifex/cmd/spinifex/cmd.Version=v1.0.0".
var (
	Version = "dev"
	Commit  = "unknown"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the Spinifex version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("spinifex %s (%s) %s/%s\n", Version, Commit, runtime.GOOS, runtime.GOARCH)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
