package handlers_ec2_launchtemplate

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// ExpandRunInstances folds a referenced launch template into input before it is
// validated: it resolves the template version, maps its stored data to a
// RunInstancesInput base, then overlays the direct RunInstances parameters
// (whole-field, presence-based — direct params win, nil inherits the template).
// MinCount/MaxCount are never sourced from a template. input.LaunchTemplate is
// cleared so the downstream launch path is untouched. It is a no-op when no
// template is referenced.
func ExpandRunInstances(ctx context.Context, svc LaunchTemplateService, input *ec2.RunInstancesInput, accountID string) error {
	if input == nil || input.LaunchTemplate == nil {
		return nil
	}
	spec := input.LaunchTemplate

	id := aws.StringValue(spec.LaunchTemplateId)
	name := aws.StringValue(spec.LaunchTemplateName)
	switch {
	case id != "" && name != "":
		return errors.New(awserrors.ErrorInvalidParameterValue)
	case id == "" && name == "":
		return errors.New(awserrors.ErrorMissingParameter)
	}

	// Version defaults to $Default; DescribeLaunchTemplateVersions resolves the
	// alias and surfaces a missing body as VersionNotFound.
	version := aws.StringValue(spec.Version)
	if version == "" {
		version = versionDefault
	}
	descIn := &ec2.DescribeLaunchTemplateVersionsInput{
		Versions: []*string{aws.String(version)},
	}
	if id != "" {
		descIn.LaunchTemplateId = aws.String(id)
	} else {
		descIn.LaunchTemplateName = aws.String(name)
	}

	out, err := svc.DescribeLaunchTemplateVersions(ctx, descIn, accountID)
	if err != nil {
		return err
	}
	if len(out.LaunchTemplateVersions) == 0 || out.LaunchTemplateVersions[0].LaunchTemplateData == nil {
		return errors.New(awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound)
	}

	base, err := responseToRunInstances(out.LaunchTemplateVersions[0].LaunchTemplateData)
	if err != nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	merged := mergeRunInstancesInput(base, input)
	merged.LaunchTemplate = nil
	*input = *merged
	return nil
}
