package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mulgadc/predastore/s3api"
	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/mulgadc/spinifex/spinifex/gateway"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"
)

func main() {
	outputDir := flag.String("out", "", "write one publishable page per service into this directory")
	jsonOutput := flag.String("json", "", "write a machine-readable coverage inventory to this file")
	flag.Parse()

	coverages, err := compareAll()
	if err != nil {
		fail(err)
	}
	if *outputDir == "" && *jsonOutput == "" {
		fmt.Print(awsmodel.RenderCoverageSummary(coverages))
		return
	}
	if *outputDir != "" {
		if err := awsmodel.WritePages(*outputDir, coverages); err != nil {
			fail(err)
		}
	}
	if *jsonOutput != "" {
		if err := awsmodel.WriteCoverageJSON(*jsonOutput, coverages); err != nil {
			fail(err)
		}
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "aws-model-coverage: %v\n", err)
	os.Exit(1)
}

func compareAll() ([]awsmodel.OperationCoverage, error) {
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
		// Predastore serves the S3 REST surface, and names the operations each
		// of its routes answers, so the comparison is the same one.
		case service == awsmodel.S3:
			modelInventory = awsmodel.DispatchInventory{Registered: s3api.Operations()}
		default:
			return nil, fmt.Errorf("no gateway inventory for %s", service)
		}

		coverage, err := awsmodel.CompareOperations(service, modelInventory)
		if err != nil {
			return nil, err
		}
		coverages = append(coverages, coverage)
	}
	return coverages, nil
}
