package gpu

import (
	"testing"
)

func TestGroupMembers_GPUWithAudioCompanion(t *testing.T) {
	root := t.TempDir()
	// GPU: 3D controller
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "nvidia", 7)
	// Companion audio device (HDMI audio on the same card)
	buildSysfsDevice(t, root, "0000:03:00.1", "0x040300", "0x10de", "0x1aef", "snd_hda_intel", 7)

	members, err := groupMembers(root, 7)
	if err != nil {
		t.Fatalf("groupMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("want 2 members, got %d", len(members))
	}

	byAddr := make(map[string]IOMMUGroupMember, len(members))
	for _, m := range members {
		byAddr[m.PCIAddress] = m
	}

	gpu, ok := byAddr["0000:03:00.0"]
	if !ok {
		t.Fatal("GPU member 0000:03:00.0 not found")
	}
	if gpu.VendorID != "10de" {
		t.Errorf("GPU VendorID = %q, want 10de", gpu.VendorID)
	}
	if gpu.DeviceID != "2236" {
		t.Errorf("GPU DeviceID = %q, want 2236", gpu.DeviceID)
	}
	if gpu.Class != "0x030200" {
		t.Errorf("GPU Class = %q, want 0x030200", gpu.Class)
	}

	audio, ok := byAddr["0000:03:00.1"]
	if !ok {
		t.Fatal("audio member 0000:03:00.1 not found")
	}
	if audio.Class != "0x040300" {
		t.Errorf("audio Class = %q, want 0x040300", audio.Class)
	}
}

func TestGroupMembers_SingleDevice(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:05:00.0", "0x030200", "0x10de", "0x2330", "nvidia", 12)

	members, err := groupMembers(root, 12)
	if err != nil {
		t.Fatalf("groupMembers: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("want 1 member, got %d", len(members))
	}
	if members[0].PCIAddress != "0000:05:00.0" {
		t.Errorf("PCIAddress = %q, want 0000:05:00.0", members[0].PCIAddress)
	}
}

func TestGroupMembers_MissingGroup(t *testing.T) {
	root := t.TempDir()
	_, err := groupMembers(root, 99)
	if err == nil {
		t.Error("want error for non-existent IOMMU group, got nil")
	}
}
