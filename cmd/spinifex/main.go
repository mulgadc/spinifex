package main

import (
	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"
	"github.com/mulgadc/spinifex/cmd/spinifex/cmd"
)

func main() {
	cmd.Execute()
}
