package utils

import (
	"os"
	"testing"
)

func TestSudoCommand_NonRoot(t *testing.T) {
	cmd := SudoCommand("ovs-vsctl", "br-exists", "br-int")
	args := cmd.Args

	if os.Getuid() == 0 {
		if args[0] != "ovs-vsctl" {
			t.Errorf("as root, expected args[0]='ovs-vsctl', got %q", args[0])
		}
		return
	}
	if args[0] != "sudo" {
		t.Errorf("as non-root, expected args[0]='sudo', got %q", args[0])
	}
	if args[1] != "ovs-vsctl" {
		t.Errorf("as non-root, expected args[1]='ovs-vsctl', got %q", args[1])
	}
	if len(args) != 4 {
		t.Errorf("expected 4 args [sudo ovs-vsctl br-exists br-int], got %d: %v", len(args), args)
	}
}
