package gateway_ec2_routetable

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

const testAccountID = "123456789012"

// CreateRouteTable tests

func TestValidateCreateRouteTableInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.CreateRouteTableInput
		wantErr string
	}{
		{"nil input", nil, awserrors.ErrorInvalidParameterValue},
		{"missing VpcId", &ec2.CreateRouteTableInput{}, awserrors.ErrorMissingParameter},
		{"empty VpcId", &ec2.CreateRouteTableInput{VpcId: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid input", &ec2.CreateRouteTableInput{VpcId: aws.String("vpc-1")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCreateRouteTableInput(tt.input)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

func TestCreateRouteTable_NilInput(t *testing.T) {
	_, err := CreateRouteTable(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateRouteTable_NilNATS(t *testing.T) {
	_, err := CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String("vpc-1")}, nil, testAccountID)
	assert.Error(t, err)
}

// DeleteRouteTable tests

func TestValidateDeleteRouteTableInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DeleteRouteTableInput
		wantErr string
	}{
		{"nil input", nil, awserrors.ErrorInvalidParameterValue},
		{"missing RouteTableId", &ec2.DeleteRouteTableInput{}, awserrors.ErrorMissingParameter},
		{"empty RouteTableId", &ec2.DeleteRouteTableInput{RouteTableId: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid input", &ec2.DeleteRouteTableInput{RouteTableId: aws.String("rtb-1")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDeleteRouteTableInput(tt.input)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

func TestDeleteRouteTable_NilInput(t *testing.T) {
	_, err := DeleteRouteTable(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDeleteRouteTable_NilNATS(t *testing.T) {
	_, err := DeleteRouteTable(&ec2.DeleteRouteTableInput{RouteTableId: aws.String("rtb-1")}, nil, testAccountID)
	assert.Error(t, err)
}

// DescribeRouteTables tests

func TestDescribeRouteTables_NilInput(t *testing.T) {
	_, err := DescribeRouteTables(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDescribeRouteTables_NilNATS(t *testing.T) {
	_, err := DescribeRouteTables(&ec2.DescribeRouteTablesInput{}, nil, testAccountID)
	assert.Error(t, err)
}

// CreateRoute tests

func TestValidateCreateRouteInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.CreateRouteInput
		wantErr string
	}{
		{"nil input", nil, awserrors.ErrorInvalidParameterValue},
		{"missing RouteTableId", &ec2.CreateRouteInput{DestinationCidrBlock: aws.String("0.0.0.0/0")}, awserrors.ErrorMissingParameter},
		{"empty RouteTableId", &ec2.CreateRouteInput{RouteTableId: aws.String(""), DestinationCidrBlock: aws.String("0.0.0.0/0")}, awserrors.ErrorMissingParameter},
		{"missing DestinationCidrBlock", &ec2.CreateRouteInput{RouteTableId: aws.String("rtb-1")}, awserrors.ErrorMissingParameter},
		{"empty DestinationCidrBlock", &ec2.CreateRouteInput{RouteTableId: aws.String("rtb-1"), DestinationCidrBlock: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid input", &ec2.CreateRouteInput{RouteTableId: aws.String("rtb-1"), DestinationCidrBlock: aws.String("0.0.0.0/0")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCreateRouteInput(tt.input)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

func TestCreateRoute_NilInput(t *testing.T) {
	_, err := CreateRoute(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateRoute_NilNATS(t *testing.T) {
	_, err := CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String("rtb-1"),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	}, nil, testAccountID)
	assert.Error(t, err)
}

// DeleteRoute tests

func TestValidateDeleteRouteInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DeleteRouteInput
		wantErr string
	}{
		{"nil input", nil, awserrors.ErrorInvalidParameterValue},
		{"missing RouteTableId", &ec2.DeleteRouteInput{DestinationCidrBlock: aws.String("0.0.0.0/0")}, awserrors.ErrorMissingParameter},
		{"missing DestinationCidrBlock", &ec2.DeleteRouteInput{RouteTableId: aws.String("rtb-1")}, awserrors.ErrorMissingParameter},
		{"valid input", &ec2.DeleteRouteInput{RouteTableId: aws.String("rtb-1"), DestinationCidrBlock: aws.String("0.0.0.0/0")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDeleteRouteInput(tt.input)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

func TestDeleteRoute_NilInput(t *testing.T) {
	_, err := DeleteRoute(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDeleteRoute_NilNATS(t *testing.T) {
	_, err := DeleteRoute(&ec2.DeleteRouteInput{
		RouteTableId:         aws.String("rtb-1"),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	}, nil, testAccountID)
	assert.Error(t, err)
}

// ReplaceRoute tests

func TestValidateReplaceRouteInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.ReplaceRouteInput
		wantErr string
	}{
		{"nil input", nil, awserrors.ErrorInvalidParameterValue},
		{"missing RouteTableId", &ec2.ReplaceRouteInput{DestinationCidrBlock: aws.String("0.0.0.0/0")}, awserrors.ErrorMissingParameter},
		{"missing DestinationCidrBlock", &ec2.ReplaceRouteInput{RouteTableId: aws.String("rtb-1")}, awserrors.ErrorMissingParameter},
		{"valid input", &ec2.ReplaceRouteInput{RouteTableId: aws.String("rtb-1"), DestinationCidrBlock: aws.String("0.0.0.0/0")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateReplaceRouteInput(tt.input)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

func TestReplaceRoute_NilInput(t *testing.T) {
	_, err := ReplaceRoute(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestReplaceRoute_NilNATS(t *testing.T) {
	_, err := ReplaceRoute(&ec2.ReplaceRouteInput{
		RouteTableId:         aws.String("rtb-1"),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	}, nil, testAccountID)
	assert.Error(t, err)
}

// AssociateRouteTable tests

func TestValidateAssociateRouteTableInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.AssociateRouteTableInput
		wantErr string
	}{
		{"nil input", nil, awserrors.ErrorInvalidParameterValue},
		{"missing RouteTableId", &ec2.AssociateRouteTableInput{SubnetId: aws.String("subnet-1")}, awserrors.ErrorMissingParameter},
		{"missing SubnetId", &ec2.AssociateRouteTableInput{RouteTableId: aws.String("rtb-1")}, awserrors.ErrorMissingParameter},
		{"valid input", &ec2.AssociateRouteTableInput{RouteTableId: aws.String("rtb-1"), SubnetId: aws.String("subnet-1")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAssociateRouteTableInput(tt.input)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

func TestAssociateRouteTable_NilInput(t *testing.T) {
	_, err := AssociateRouteTable(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestAssociateRouteTable_NilNATS(t *testing.T) {
	_, err := AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String("rtb-1"),
		SubnetId:     aws.String("subnet-1"),
	}, nil, testAccountID)
	assert.Error(t, err)
}

// DisassociateRouteTable tests

func TestValidateDisassociateRouteTableInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DisassociateRouteTableInput
		wantErr string
	}{
		{"nil input", nil, awserrors.ErrorInvalidParameterValue},
		{"missing AssociationId", &ec2.DisassociateRouteTableInput{}, awserrors.ErrorMissingParameter},
		{"empty AssociationId", &ec2.DisassociateRouteTableInput{AssociationId: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid input", &ec2.DisassociateRouteTableInput{AssociationId: aws.String("rtbassoc-1")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDisassociateRouteTableInput(tt.input)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

func TestDisassociateRouteTable_NilInput(t *testing.T) {
	_, err := DisassociateRouteTable(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDisassociateRouteTable_NilNATS(t *testing.T) {
	_, err := DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
		AssociationId: aws.String("rtbassoc-1"),
	}, nil, testAccountID)
	assert.Error(t, err)
}

// ReplaceRouteTableAssociation tests

func TestValidateReplaceRouteTableAssociationInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.ReplaceRouteTableAssociationInput
		wantErr string
	}{
		{"nil input", nil, awserrors.ErrorInvalidParameterValue},
		{"missing AssociationId", &ec2.ReplaceRouteTableAssociationInput{RouteTableId: aws.String("rtb-1")}, awserrors.ErrorMissingParameter},
		{"missing RouteTableId", &ec2.ReplaceRouteTableAssociationInput{AssociationId: aws.String("rtbassoc-1")}, awserrors.ErrorMissingParameter},
		{"valid input", &ec2.ReplaceRouteTableAssociationInput{AssociationId: aws.String("rtbassoc-1"), RouteTableId: aws.String("rtb-1")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateReplaceRouteTableAssociationInput(tt.input)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

func TestReplaceRouteTableAssociation_NilInput(t *testing.T) {
	_, err := ReplaceRouteTableAssociation(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestReplaceRouteTableAssociation_NilNATS(t *testing.T) {
	_, err := ReplaceRouteTableAssociation(&ec2.ReplaceRouteTableAssociationInput{
		AssociationId: aws.String("rtbassoc-1"),
		RouteTableId:  aws.String("rtb-1"),
	}, nil, testAccountID)
	assert.Error(t, err)
}
