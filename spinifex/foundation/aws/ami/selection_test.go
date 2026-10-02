package ami

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

func imageForTest(id, created string, tagKeys ...string) *ec2.Image {
	image := &ec2.Image{ImageId: aws.String(id), CreationDate: aws.String(created)}
	for _, key := range tagKeys {
		if key == "" {
			image.Tags = append(image.Tags, nil)
			continue
		}
		image.Tags = append(image.Tags, &ec2.Tag{Key: aws.String(key), Value: aws.String("v")})
	}
	return image
}

func TestSelectNewestImage(t *testing.T) {
	tests := []struct {
		name        string
		images      []*ec2.Image
		excludeKey  string
		wantID      string
		wantCreated string
		wantMatches int
	}{
		{
			name: "empty input",
		},
		{
			name:   "all entries nil",
			images: []*ec2.Image{nil, nil},
		},
		{
			name:   "empty image id skipped",
			images: []*ec2.Image{imageForTest("", "2026-01-01T00:00:00.000Z"), imageForTest("ami-a", "2025-01-01T00:00:00.000Z")},
			// The empty-ID entry is not a match, so no multi-match warning fires.
			wantID: "ami-a", wantCreated: "2025-01-01T00:00:00.000Z", wantMatches: 1,
		},
		{
			name:   "newest creation date wins regardless of order",
			images: []*ec2.Image{imageForTest("ami-old", "2026-01-01T00:00:00.000Z"), imageForTest("ami-new", "2026-08-01T00:00:00.000Z"), imageForTest("ami-mid", "2026-04-01T00:00:00.000Z")},
			wantID: "ami-new", wantCreated: "2026-08-01T00:00:00.000Z", wantMatches: 3,
		},
		{
			name:   "identical dates keep the first match",
			images: []*ec2.Image{imageForTest("ami-a", "2026-01-01T00:00:00.000Z"), imageForTest("ami-b", "2026-01-01T00:00:00.000Z")},
			wantID: "ami-a", wantCreated: "2026-01-01T00:00:00.000Z", wantMatches: 2,
		},
		{
			name:       "exclude tag key drops tagged images",
			images:     []*ec2.Image{imageForTest("ami-gpu", "2026-08-01T00:00:00.000Z", "gpu-vendor"), imageForTest("ami-cpu", "2026-01-01T00:00:00.000Z")},
			excludeKey: "gpu-vendor",
			wantID:     "ami-cpu", wantCreated: "2026-01-01T00:00:00.000Z", wantMatches: 1,
		},
		{
			name:   "empty exclude key keeps tagged images",
			images: []*ec2.Image{imageForTest("ami-gpu", "2026-08-01T00:00:00.000Z", "gpu-vendor"), imageForTest("ami-cpu", "2026-01-01T00:00:00.000Z")},
			wantID: "ami-gpu", wantCreated: "2026-08-01T00:00:00.000Z", wantMatches: 2,
		},
		{
			name:       "all images excluded",
			images:     []*ec2.Image{imageForTest("ami-gpu", "2026-08-01T00:00:00.000Z", "gpu-vendor")},
			excludeKey: "gpu-vendor",
		},
		{
			name:       "nil tag entry does not panic",
			images:     []*ec2.Image{imageForTest("ami-a", "2026-01-01T00:00:00.000Z", "", "gpu-vendor"), imageForTest("ami-b", "2026-02-01T00:00:00.000Z", "")},
			excludeKey: "gpu-vendor",
			wantID:     "ami-b", wantCreated: "2026-02-01T00:00:00.000Z", wantMatches: 1,
		},
		{
			name:   "missing creation date sorts oldest",
			images: []*ec2.Image{{ImageId: aws.String("ami-nodate")}, imageForTest("ami-dated", "2020-01-01T00:00:00.000Z")},
			wantID: "ami-dated", wantCreated: "2020-01-01T00:00:00.000Z", wantMatches: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotCreated, gotMatches := SelectNewestImage(tt.images, tt.excludeKey)
			if gotID != tt.wantID {
				t.Errorf("imageID = %q, want %q", gotID, tt.wantID)
			}
			if gotCreated != tt.wantCreated {
				t.Errorf("created = %q, want %q", gotCreated, tt.wantCreated)
			}
			if gotMatches != tt.wantMatches {
				t.Errorf("matches = %d, want %d", gotMatches, tt.wantMatches)
			}
		})
	}
}
