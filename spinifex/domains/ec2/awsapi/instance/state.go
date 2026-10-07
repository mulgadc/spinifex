package instance

import "github.com/aws/aws-sdk-go/service/ec2"

func newStateChange(instanceID string, currentCode int64, currentName string, prevCode int64, prevName string) *ec2.InstanceStateChange {
	return &ec2.InstanceStateChange{
		InstanceId:    &instanceID,
		CurrentState:  &ec2.InstanceState{Code: &currentCode, Name: &currentName},
		PreviousState: &ec2.InstanceState{Code: &prevCode, Name: &prevName},
	}
}
