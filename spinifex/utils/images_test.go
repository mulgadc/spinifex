package utils

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// resolveServingAMI filters purely on these two tags and never on the image
// name, so a typo in either hides the serving image from Ochre and every
// endpoint launch dies at "bedrock: no vllm-serving AMI found".
func TestAvailableImages_VLLMServingCarriesBedrockTags(t *testing.T) {
	const key = "ubuntu-26.04-vllm-serving-x86_64"

	img, ok := AvailableImages[key]
	if !ok {
		t.Fatalf("%s missing from AvailableImages", key)
	}

	for tag, want := range map[string]string{
		"spinifex:managed-by":   "bedrock",
		"spinifex:bedrock-role": "vllm-serving",
	} {
		if got := img.Tags[tag]; got != want {
			t.Errorf("tag %q = %q, want %q", tag, got, want)
		}
	}

	// The map key and Name are matched independently by different callers, so
	// they drifting apart is a real failure mode rather than a tautology.
	if img.Name != key {
		t.Errorf("Name = %q, want %q", img.Name, key)
	}
	if img.BootMode != "uefi" {
		t.Errorf("BootMode = %q, want uefi", img.BootMode)
	}
	if img.Arch != "x86_64" {
		t.Errorf("Arch = %q, want x86_64", img.Arch)
	}
	if img.URL == "" || img.Checksum == "" {
		t.Error("URL and Checksum must both be set or the import has nothing to fetch and verify")
	}
}

func img(id, created string, tagKeys ...string) *ec2.Image {
	image := &ec2.Image{ImageId: aws.String(id), CreationDate: aws.String(created)}
	for _, k := range tagKeys {
		if k == "" {
			image.Tags = append(image.Tags, nil)
			continue
		}
		image.Tags = append(image.Tags, &ec2.Tag{Key: aws.String(k), Value: aws.String("v")})
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
			images: []*ec2.Image{img("", "2026-01-01T00:00:00.000Z"), img("ami-a", "2025-01-01T00:00:00.000Z")},
			// The empty-ID entry is not a match, so no multi-match warning fires.
			wantID: "ami-a", wantCreated: "2025-01-01T00:00:00.000Z", wantMatches: 1,
		},
		{
			name:   "newest creation date wins regardless of order",
			images: []*ec2.Image{img("ami-old", "2026-01-01T00:00:00.000Z"), img("ami-new", "2026-08-01T00:00:00.000Z"), img("ami-mid", "2026-04-01T00:00:00.000Z")},
			wantID: "ami-new", wantCreated: "2026-08-01T00:00:00.000Z", wantMatches: 3,
		},
		{
			name:   "identical dates keep the first match",
			images: []*ec2.Image{img("ami-a", "2026-01-01T00:00:00.000Z"), img("ami-b", "2026-01-01T00:00:00.000Z")},
			wantID: "ami-a", wantCreated: "2026-01-01T00:00:00.000Z", wantMatches: 2,
		},
		{
			name:       "exclude tag key drops tagged images",
			images:     []*ec2.Image{img("ami-gpu", "2026-08-01T00:00:00.000Z", "gpu-vendor"), img("ami-cpu", "2026-01-01T00:00:00.000Z")},
			excludeKey: "gpu-vendor",
			wantID:     "ami-cpu", wantCreated: "2026-01-01T00:00:00.000Z", wantMatches: 1,
		},
		{
			name:   "empty exclude key keeps tagged images",
			images: []*ec2.Image{img("ami-gpu", "2026-08-01T00:00:00.000Z", "gpu-vendor"), img("ami-cpu", "2026-01-01T00:00:00.000Z")},
			wantID: "ami-gpu", wantCreated: "2026-08-01T00:00:00.000Z", wantMatches: 2,
		},
		{
			name:       "all images excluded",
			images:     []*ec2.Image{img("ami-gpu", "2026-08-01T00:00:00.000Z", "gpu-vendor")},
			excludeKey: "gpu-vendor",
		},
		{
			name:       "nil tag entry does not panic",
			images:     []*ec2.Image{img("ami-a", "2026-01-01T00:00:00.000Z", "", "gpu-vendor"), img("ami-b", "2026-02-01T00:00:00.000Z", "")},
			excludeKey: "gpu-vendor",
			wantID:     "ami-b", wantCreated: "2026-02-01T00:00:00.000Z", wantMatches: 1,
		},
		{
			name:   "missing creation date sorts oldest",
			images: []*ec2.Image{&ec2.Image{ImageId: aws.String("ami-nodate")}, img("ami-dated", "2020-01-01T00:00:00.000Z")},
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
