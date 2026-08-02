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
