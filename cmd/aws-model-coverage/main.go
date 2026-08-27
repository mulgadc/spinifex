package main

import (
	"fmt"
	"os"

	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/mulgadc/spinifex/spinifex/gateway"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"
)

func main() {
	dispatch := gateway.AWSOperationInventory()
	coverages := make([]awsmodel.OperationCoverage, 0, len(awsmodel.Services()))
	for _, service := range awsmodel.Services() {
		inventory, ok := dispatch[string(service)]
		var modelInventory awsmodel.DispatchInventory
		switch {
		case ok:
			modelInventory = awsmodel.DispatchInventory{
				Registered:  inventory.Registered,
				Stubbed:     inventory.Stubbed,
				Unsupported: inventory.Unsupported,
			}
		case service == awsmodel.S3:
			modelInventory = awsmodel.DispatchInventory{
				Opaque: true,
				Note:   "Spinifex delegates the S3 REST surface to Predastore, which has no operation-name dispatch table to compare mechanically.",
			}
		default:
			fmt.Fprintf(os.Stderr, "aws-model-coverage: no gateway inventory for %s\n", service)
			os.Exit(1)
		}

		coverage, err := awsmodel.CompareOperations(service, modelInventory)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aws-model-coverage: %v\n", err)
			os.Exit(1)
		}
		coverages = append(coverages, coverage)
	}

	fmt.Print(awsmodel.RenderCoverageMarkdown(coverages))
}
