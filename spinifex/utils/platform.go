package utils

import "strings"

// PlatformWindows is the only value AWS ever puts in the ec2 Platform field.
const PlatformWindows = "windows"

// PlatformFromDetails derives the AWS Platform field from PlatformDetails,
// returning nil for anything that is not Windows.
//
// The two fields are not interchangeable: PlatformDetails is a billing string
// covering every OS ("Linux/UNIX", "Red Hat Enterprise Linux", "Windows",
// "Windows BYOL"), while Platform is either "windows" or absent. The AWS SDKs
// and the Terraform provider key off Platform to decide an image or instance is
// Windows, so a Windows AMI that omits it reads as Linux.
func PlatformFromDetails(platformDetails string) *string {
	if !strings.HasPrefix(strings.ToLower(platformDetails), PlatformWindows) {
		return nil
	}
	p := PlatformWindows
	return &p
}

// usageOperations maps PlatformDetails to the billing code AWS reports as an
// image's UsageOperation, for the platforms checked against real AWS images.
var usageOperations = map[string]string{
	"Linux/UNIX":               "RunInstances",
	"Red Hat Enterprise Linux": "RunInstances:0010",
	"SUSE Linux":               "RunInstances:000g",
	"Ubuntu Pro Linux":         "RunInstances:0g00",
	"Windows":                  "RunInstances:0002",
}

// UsageOperationFromDetails returns the UsageOperation for platformDetails, or
// nil for a platform whose code has not been confirmed.
func UsageOperationFromDetails(platformDetails string) *string {
	op, ok := usageOperations[platformDetails]
	if !ok {
		return nil
	}
	return &op
}
