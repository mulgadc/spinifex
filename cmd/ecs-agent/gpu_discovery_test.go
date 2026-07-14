package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseNvidiaSMIUUIDs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   \n\n  ", nil},
		{"single", "GPU-11111111-1111-1111-1111-111111111111\n", []string{"GPU-11111111-1111-1111-1111-111111111111"}},
		{"multi", "GPU-aaa\nGPU-bbb\nGPU-ccc\n", []string{"GPU-aaa", "GPU-bbb", "GPU-ccc"}},
		{"blank lines between", "GPU-aaa\n\nGPU-bbb\n", []string{"GPU-aaa", "GPU-bbb"}},
		{"trailing whitespace per line", "GPU-aaa \r\nGPU-bbb\r\n", []string{"GPU-aaa", "GPU-bbb"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseNvidiaSMIUUIDs(c.in)
			if !reflect.DeepEqual(got, c.want) && (len(got) != 0 || len(c.want) != 0) {
				t.Errorf("parseNvidiaSMIUUIDs(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestDiscoverNvidiaGPUs_AbsentBinaryReturnsEmptyNoError(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		return nil, errors.New(`exec: "nvidia-smi": executable file not found in $PATH`)
	}
	got := discoverNvidiaGPUs(run)
	if len(got) != 0 {
		t.Errorf("want empty list on absent nvidia-smi, got %v", got)
	}
}

func TestDiscoverNvidiaGPUs_ParsesFixtureOutput(t *testing.T) {
	var gotName string
	var gotArgs []string
	run := func(name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = args
		return []byte("GPU-aaa\nGPU-bbb\n"), nil
	}
	got := discoverNvidiaGPUs(run)
	want := []string{"GPU-aaa", "GPU-bbb"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if gotName != "nvidia-smi" {
		t.Errorf("command = %q, want nvidia-smi", gotName)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "--query-gpu=uuid" || gotArgs[1] != "--format=csv,noheader" {
		t.Errorf("args = %v, want the uuid/csv,noheader query", gotArgs)
	}
}

func TestDiscoverNvidiaGPUs_NoDevicesReturnsEmpty(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		return []byte(""), nil
	}
	got := discoverNvidiaGPUs(run)
	if len(got) != 0 {
		t.Errorf("want empty list, got %v", got)
	}
}
