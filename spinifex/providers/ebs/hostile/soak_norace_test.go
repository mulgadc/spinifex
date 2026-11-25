//go:build !race

package hostile_test

// Volumes per worker for the default soak runs in a normal build.
const (
	leakSoakVolumesPerWorker  = 3600
	matchSoakVolumesPerWorker = 1400
)
