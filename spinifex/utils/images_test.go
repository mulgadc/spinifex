package utils

import "testing"

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
