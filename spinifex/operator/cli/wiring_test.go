package cli

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
)

func TestMustBindPFlag_PanicsNamingKeyAndFlagWhenUndeclared(t *testing.T) {
	flags := pflag.NewFlagSet("declares-other", pflag.ContinueOnError)
	flags.String("other", "", "")
	assert.PanicsWithValue(t, `bind config key "s3-host" to flag "s3-host": flag for "s3-host" is nil`, func() {
		mustBindPFlag("s3-host", flags, "s3-host")
	})
}

func TestMustMarkFlagRequired_PanicsNamingFlagAndCommandWhenUndeclared(t *testing.T) {
	c := &cobra.Command{Use: "probe"}
	assert.PanicsWithValue(t, `mark flag "model-id" required on "probe": no such flag -model-id`, func() {
		mustMarkFlagRequired(c, "model-id")
	})
}
