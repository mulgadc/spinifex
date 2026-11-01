package awsgw_test

import (
	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
	taggingawsapi "github.com/mulgadc/spinifex/spinifex/domains/tagging/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
)

// withTagging registers the Resource Groups Tagging API on gw backed by agg, as
// the awsgw role composes it.
func withTagging(gw *pipeline.Config, agg *tagging.Aggregator) *pipeline.Config {
	return withServices(gw, taggingRegistration(agg))
}

// taggingRegistration is the Tagging registration over agg.
func taggingRegistration(agg *tagging.Aggregator) dispatch.Registration {
	return taggingawsapi.NewRegistration(taggingawsapi.Deps{Aggregator: agg})
}
