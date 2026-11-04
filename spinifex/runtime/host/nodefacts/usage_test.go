package nodefacts

import (
	"sync/atomic"
	"testing"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// A 4 KiB-block filesystem of 1000 blocks: 600 free, 500 of them available to
// an unprivileged user, as df counts.
func fakeStatfs(_ string, st *unix.Statfs_t) error {
	st.Bsize, st.Frsize = 4096, 4096
	st.Blocks, st.Bfree, st.Bavail = 1000, 600, 500
	return nil
}

func TestUsageFillsSizesAndReportsAnUnreadableMount(t *testing.T) {
	p := &UsageProber{Statfs: func(path string, st *unix.Statfs_t) error {
		if path == "/private" {
			return unix.EACCES
		}
		return fakeStatfs(path, st)
	}}
	fs := []clusterv1.FilesystemFacts{{Mount: "/"}, {Mount: "/private"}, {Mount: "/new"}}

	p.Probe([]string{"/", "/private"})
	p.Apply(fs)

	assert.Equal(t, clusterv1.FilesystemFacts{Mount: "/", SizeBytes: 1000 * 4096, UsedBytes: 400 * 4096, AvailBytes: 500 * 4096}, fs[0])
	assert.Equal(t, clusterv1.FilesystemFacts{Mount: "/private", Error: "permission denied"}, fs[1])
	assert.Equal(t, "not read yet", fs[2].Error)
}

func TestUsageReportsAHungMountWithoutWaitingOrProbingItTwice(t *testing.T) {
	release := make(chan struct{})
	var hungCalls atomic.Int32
	hang := false
	p := &UsageProber{Deadline: 20 * time.Millisecond, Statfs: func(path string, st *unix.Statfs_t) error {
		if path == "/hung" && hang {
			hungCalls.Add(1)
			<-release
		}
		return fakeStatfs(path, st)
	}}
	p.Probe([]string{"/", "/hung"})
	hang = true

	start := time.Now()
	p.Probe([]string{"/", "/hung"})
	p.Probe([]string{"/", "/hung"})
	assert.Less(t, time.Since(start), time.Second, "Probe returns at its deadline")
	assert.Equal(t, int32(1), hungCalls.Load(), "a mount still being read is not read again")

	fs := []clusterv1.FilesystemFacts{{Mount: "/"}, {Mount: "/hung"}}
	p.Apply(fs)
	assert.Empty(t, fs[0].Error)
	assert.Contains(t, fs[1].Error, "not answering; sizes as of ")
	assert.Equal(t, uint64(1000*4096), fs[1].SizeBytes, "the last good reading is kept")

	close(release)
	require.Eventually(t, func() bool {
		fs := []clusterv1.FilesystemFacts{{Mount: "/hung"}}
		p.Apply(fs)
		return fs[0].Error == ""
	}, 5*time.Second, 10*time.Millisecond)
}

func TestUsageReportsAMountThatNeverAnswered(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	p := &UsageProber{Deadline: 10 * time.Millisecond, Statfs: func(string, *unix.Statfs_t) error {
		<-release
		return nil
	}}

	p.Probe([]string{"/hung"})

	fs := []clusterv1.FilesystemFacts{{Mount: "/hung"}}
	p.Apply(fs)
	assert.Equal(t, clusterv1.FilesystemFacts{Mount: "/hung", Error: "not answering"}, fs[0])
}
