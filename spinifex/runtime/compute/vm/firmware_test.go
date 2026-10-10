package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirmwarePaths(t *testing.T) {
	dir := t.TempDir()
	codeA := filepath.Join(dir, "a-CODE.fd")
	varsA := filepath.Join(dir, "a-VARS.fd")
	codeB := filepath.Join(dir, "b-CODE.fd")
	varsB := filepath.Join(dir, "b-VARS.fd")
	require.NoError(t, os.WriteFile(codeA, []byte("code-a"), 0o644))
	require.NoError(t, os.WriteFile(varsA, make([]byte, 540_672), 0o644))
	require.NoError(t, os.WriteFile(codeB, []byte("code-b"), 0o644))
	require.NoError(t, os.WriteFile(varsB, make([]byte, 100), 0o644))

	tests := []struct {
		name        string
		candidates  map[string][]FirmwareCandidate
		arch        string
		wantCode    string
		wantVars    string
		wantSize    int64
		wantErrSubs string
	}{
		{
			name: "first match wins (distro path beats edk2 fallback)",
			candidates: map[string][]FirmwareCandidate{
				"x86_64": {
					{Code: codeA, VarsTemplate: varsA},
					{Code: codeB, VarsTemplate: varsB},
				},
			},
			arch:     "x86_64",
			wantCode: codeA,
			wantVars: varsA,
			wantSize: 540_672,
		},
		{
			name: "falls through when first code missing",
			candidates: map[string][]FirmwareCandidate{
				"x86_64": {
					{Code: filepath.Join(dir, "missing-CODE.fd"), VarsTemplate: varsA},
					{Code: codeB, VarsTemplate: varsB},
				},
			},
			arch:     "x86_64",
			wantCode: codeB,
			wantVars: varsB,
			wantSize: 100,
		},
		{
			name: "falls through when vars template missing",
			candidates: map[string][]FirmwareCandidate{
				"x86_64": {
					{Code: codeA, VarsTemplate: filepath.Join(dir, "missing-VARS.fd")},
					{Code: codeB, VarsTemplate: varsB},
				},
			},
			arch:     "x86_64",
			wantCode: codeB,
			wantVars: varsB,
			wantSize: 100,
		},
		{
			name: "unknown architecture surfaces in error",
			candidates: map[string][]FirmwareCandidate{
				"x86_64": {{Code: codeA, VarsTemplate: varsA}},
			},
			arch:        "riscv64",
			wantErrSubs: `architecture "riscv64"`,
		},
		{
			name: "no candidates match",
			candidates: map[string][]FirmwareCandidate{
				"x86_64": {{Code: "/nope/CODE.fd", VarsTemplate: "/nope/VARS.fd"}},
			},
			arch:        "x86_64",
			wantErrSubs: `architecture "x86_64"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := FirmwarePathCandidates
			FirmwarePathCandidates = tt.candidates
			t.Cleanup(func() { FirmwarePathCandidates = orig })

			code, vars, size, err := FirmwarePaths(tt.arch)
			if tt.wantErrSubs != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrSubs)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantCode, code)
			assert.Equal(t, tt.wantVars, vars)
			assert.Equal(t, tt.wantSize, size)
		})
	}
}
