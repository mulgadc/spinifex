package main

import (
	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"
	"github.com/mulgadc/spinifex/spinifex/operator/cli"
)

func main() {
	cli.Execute()
}
