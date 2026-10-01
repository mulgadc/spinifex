package gateway_ec2_account

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeAccountAttributes_AllAttributes(t *testing.T) {
	input := &ec2.DescribeAccountAttributesInput{}

	output, err := DescribeAccountAttributes(input, noDefaultVPC)
	require.NoError(t, err)
	require.NotNil(t, output)

	assert.Len(t, output.AccountAttributes, 6)

	attrMap := make(map[string]string)
	for _, attr := range output.AccountAttributes {
		attrMap[*attr.AttributeName] = *attr.AttributeValues[0].AttributeValue
	}

	assert.Equal(t, "VPC", attrMap["supported-platforms"])
	assert.Equal(t, "none", attrMap["default-vpc"])
	assert.Equal(t, "100", attrMap["max-instances"])
	assert.Equal(t, "5", attrMap["vpc-max-security-groups-per-interface"])
	assert.Equal(t, "5", attrMap["max-elastic-ips"])
	assert.Equal(t, "20", attrMap["vpc-max-elastic-ips"])
}

func TestDescribeAccountAttributes_FilterSingle(t *testing.T) {
	input := &ec2.DescribeAccountAttributesInput{
		AttributeNames: []*string{aws.String("max-instances")},
	}

	output, err := DescribeAccountAttributes(input, noDefaultVPC)
	require.NoError(t, err)
	require.NotNil(t, output)

	assert.Len(t, output.AccountAttributes, 1)
	assert.Equal(t, "max-instances", *output.AccountAttributes[0].AttributeName)
	assert.Equal(t, "100", *output.AccountAttributes[0].AttributeValues[0].AttributeValue)
}

func TestDescribeAccountAttributes_FilterMultiple(t *testing.T) {
	input := &ec2.DescribeAccountAttributesInput{
		AttributeNames: []*string{
			aws.String("supported-platforms"),
			aws.String("default-vpc"),
		},
	}

	output, err := DescribeAccountAttributes(input, noDefaultVPC)
	require.NoError(t, err)
	require.NotNil(t, output)

	assert.Len(t, output.AccountAttributes, 2)

	names := make(map[string]bool)
	for _, attr := range output.AccountAttributes {
		names[*attr.AttributeName] = true
	}
	assert.True(t, names["supported-platforms"])
	assert.True(t, names["default-vpc"])
}

func TestDescribeAccountAttributes_FilterNonExistent(t *testing.T) {
	input := &ec2.DescribeAccountAttributesInput{
		AttributeNames: []*string{aws.String("nonexistent-attribute")},
	}

	output, err := DescribeAccountAttributes(input, noDefaultVPC)
	require.NoError(t, err)
	require.NotNil(t, output)

	assert.Empty(t, output.AccountAttributes)
}

func TestDescribeAccountAttributes_EmptyAttributeNames(t *testing.T) {
	input := &ec2.DescribeAccountAttributesInput{
		AttributeNames: []*string{},
	}

	output, err := DescribeAccountAttributes(input, noDefaultVPC)
	require.NoError(t, err)
	require.NotNil(t, output)

	// Empty slice means return all
	assert.Len(t, output.AccountAttributes, 6)
}

func noDefaultVPC() (string, error) { return "", nil }

func TestDescribeAccountAttributes_DefaultVPC(t *testing.T) {
	named := &ec2.DescribeAccountAttributesInput{AttributeNames: []*string{aws.String("default-vpc")}}

	output, err := DescribeAccountAttributes(named, func() (string, error) { return "vpc-0abc", nil })
	require.NoError(t, err)
	require.Len(t, output.AccountAttributes, 1)
	assert.Equal(t, "vpc-0abc", aws.StringValue(output.AccountAttributes[0].AttributeValues[0].AttributeValue))

	output, err = DescribeAccountAttributes(named, noDefaultVPC)
	require.NoError(t, err)
	assert.Equal(t, "none", aws.StringValue(output.AccountAttributes[0].AttributeValues[0].AttributeValue))
}

// A failed lookup fails the request rather than reporting "none" for an
// account that may well have a default VPC; other attributes never look.
func TestDescribeAccountAttributes_DefaultVPCLookupFailure(t *testing.T) {
	failing := func() (string, error) { return "", errors.New(awserrors.ErrorServerInternal) }

	_, err := DescribeAccountAttributes(&ec2.DescribeAccountAttributesInput{}, failing)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())

	output, err := DescribeAccountAttributes(&ec2.DescribeAccountAttributesInput{
		AttributeNames: []*string{aws.String("max-instances")},
	}, failing)
	require.NoError(t, err)
	assert.Len(t, output.AccountAttributes, 1)
}

func TestValidateEnableEbsEncryptionByDefaultInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.EnableEbsEncryptionByDefaultInput
		wantErr bool
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
		},
		{
			name:    "ValidInput",
			input:   &ec2.EnableEbsEncryptionByDefaultInput{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEnableEbsEncryptionByDefaultInput(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateDisableEbsEncryptionByDefaultInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DisableEbsEncryptionByDefaultInput
		wantErr bool
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
		},
		{
			name:    "ValidInput",
			input:   &ec2.DisableEbsEncryptionByDefaultInput{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDisableEbsEncryptionByDefaultInput(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateGetEbsEncryptionByDefaultInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.GetEbsEncryptionByDefaultInput
		wantErr bool
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
		},
		{
			name:    "ValidInput",
			input:   &ec2.GetEbsEncryptionByDefaultInput{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGetEbsEncryptionByDefaultInput(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateEnableSerialConsoleAccessInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.EnableSerialConsoleAccessInput
		wantErr bool
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
		},
		{
			name:    "ValidInput",
			input:   &ec2.EnableSerialConsoleAccessInput{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEnableSerialConsoleAccessInput(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateDisableSerialConsoleAccessInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DisableSerialConsoleAccessInput
		wantErr bool
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
		},
		{
			name:    "ValidInput",
			input:   &ec2.DisableSerialConsoleAccessInput{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDisableSerialConsoleAccessInput(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateGetSerialConsoleAccessStatusInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.GetSerialConsoleAccessStatusInput
		wantErr bool
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
		},
		{
			name:    "ValidInput",
			input:   &ec2.GetSerialConsoleAccessStatusInput{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGetSerialConsoleAccessStatusInput(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// Handler tests — call handlers with valid input + nil NATS to cover error paths

func TestEnableEbsEncryptionByDefault_ValidationErrors(t *testing.T) {
	_, err := EnableEbsEncryptionByDefault(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestEnableEbsEncryptionByDefault_NilNATS(t *testing.T) {
	_, err := EnableEbsEncryptionByDefault(context.Background(), &ec2.EnableEbsEncryptionByDefaultInput{}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDisableEbsEncryptionByDefault_ValidationErrors(t *testing.T) {
	_, err := DisableEbsEncryptionByDefault(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDisableEbsEncryptionByDefault_NilNATS(t *testing.T) {
	_, err := DisableEbsEncryptionByDefault(context.Background(), &ec2.DisableEbsEncryptionByDefaultInput{}, nil, "acct-123")
	assert.Error(t, err)
}

func TestGetEbsEncryptionByDefault_ValidationErrors(t *testing.T) {
	_, err := GetEbsEncryptionByDefault(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestGetEbsEncryptionByDefault_NilNATS(t *testing.T) {
	_, err := GetEbsEncryptionByDefault(context.Background(), &ec2.GetEbsEncryptionByDefaultInput{}, nil, "acct-123")
	assert.Error(t, err)
}

func TestEnableSerialConsoleAccess_ValidationErrors(t *testing.T) {
	_, err := EnableSerialConsoleAccess(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestEnableSerialConsoleAccess_NilNATS(t *testing.T) {
	_, err := EnableSerialConsoleAccess(context.Background(), &ec2.EnableSerialConsoleAccessInput{}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDisableSerialConsoleAccess_ValidationErrors(t *testing.T) {
	_, err := DisableSerialConsoleAccess(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDisableSerialConsoleAccess_NilNATS(t *testing.T) {
	_, err := DisableSerialConsoleAccess(context.Background(), &ec2.DisableSerialConsoleAccessInput{}, nil, "acct-123")
	assert.Error(t, err)
}

func TestGetSerialConsoleAccessStatus_ValidationErrors(t *testing.T) {
	_, err := GetSerialConsoleAccessStatus(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestGetSerialConsoleAccessStatus_NilNATS(t *testing.T) {
	_, err := GetSerialConsoleAccessStatus(context.Background(), &ec2.GetSerialConsoleAccessStatusInput{}, nil, "acct-123")
	assert.Error(t, err)
}
