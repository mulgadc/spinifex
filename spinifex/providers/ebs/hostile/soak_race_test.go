//go:build race

package hostile_test

// The race detector makes each volume several times more costly, so a race
// build runs a smaller default soak.
const (
	leakSoakVolumesPerWorker  = 2400
	matchSoakVolumesPerWorker = 560
)
