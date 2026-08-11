package handlers_bedrock

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubImageDescriber answers DescribeImages with a fixed set of images,
// letting a test drive resolveServingAMI's selection logic directly rather
// than through the fixed single-image fakeAMIResolver launch tests use.
type stubImageDescriber struct {
	images []*ec2.Image
}

func (s stubImageDescriber) DescribeImages(_ context.Context, _ *ec2.DescribeImagesInput, _ string) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{Images: s.images}, nil
}

func image(id, created string) *ec2.Image {
	return &ec2.Image{ImageId: aws.String(id), CreationDate: aws.String(created)}
}

func TestResolveServingAMI(t *testing.T) {
	tests := []struct {
		name    string
		images  []*ec2.Image
		wantID  string
		wantErr error
	}{
		{
			name: "single match is returned",
			images: []*ec2.Image{
				image("ami-only", "2026-01-01T00:00:00.000Z"),
			},
			wantID: "ami-only",
		},
		{
			name: "multiple matches: newest first in describe order",
			images: []*ec2.Image{
				image("ami-newest", "2026-03-01T00:00:00.000Z"),
				image("ami-middle", "2026-02-01T00:00:00.000Z"),
				image("ami-oldest", "2026-01-01T00:00:00.000Z"),
			},
			wantID: "ami-newest",
		},
		{
			name: "multiple matches: newest last in describe order",
			images: []*ec2.Image{
				image("ami-oldest", "2026-01-01T00:00:00.000Z"),
				image("ami-middle", "2026-02-01T00:00:00.000Z"),
				image("ami-newest", "2026-03-01T00:00:00.000Z"),
			},
			wantID: "ami-newest",
		},
		{
			name:    "zero matches",
			images:  nil,
			wantErr: ErrServingAMINotFound,
		},
		{
			name: "nil and empty ImageId entries are skipped",
			images: []*ec2.Image{
				nil,
				{ImageId: nil, CreationDate: aws.String("2026-01-01T00:00:00.000Z")},
				{ImageId: aws.String(""), CreationDate: aws.String("2026-01-01T00:00:00.000Z")},
				image("ami-valid", "2025-01-01T00:00:00.000Z"),
			},
			wantID: "ami-valid",
		},
		{
			name: "only nil/empty ImageId entries: not found",
			images: []*ec2.Image{
				nil,
				{ImageId: nil},
				{ImageId: aws.String("")},
			},
			wantErr: ErrServingAMINotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, err := resolveServingAMI(context.Background(), stubImageDescriber{images: tt.images})
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Empty(t, gotID)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantID, gotID)
		})
	}
}
