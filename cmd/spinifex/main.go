package main

import (
	"github.com/mulgadc/spinifex/cmd/spinifex/cmd"
	_ "github.com/mulgadc/spinifex/internal/fipsboot"
)

func main() {
	cmd.Execute()
}
