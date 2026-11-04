package nodefacts

import (
	"encoding/json"
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
	fs := []clusterv1.FilesystemFacts{{Mount: "/", ID: "1"}, {Mount: "/private", ID: "2"}, {Mount: "/new", ID: "3"}, {Mount: "/home", Error: notVisible}}

	p.Probe([]Mount{{"1", "/"}, {"2", "/private"}})
	p.Apply(fs)

	assert.Equal(t, &clusterv1.FilesystemUsage{SizeBytes: 1000 * 4096, UsedBytes: 400 * 4096, AvailBytes: 500 * 4096}, fs[0].Usage)
	assert.Empty(t, fs[0].Error)
	assert.Equal(t, clusterv1.FilesystemFacts{Mount: "/private", ID: "2", Error: "permission denied"}, fs[1])
	assert.Equal(t, "not read yet", fs[2].Error)
	assert.Equal(t, clusterv1.FilesystemFacts{Mount: "/home", Error: notVisible}, fs[3], "a hidden mount is not given another's usage")
}

func TestUsageSendsZeroSpaceOnAFullFilesystem(t *testing.T) {
	p := &UsageProber{Statfs: func(_ string, st *unix.Statfs_t) error {
		st.Frsize, st.Blocks = 4096, 1000
		return nil
	}}
	fs := []clusterv1.FilesystemFacts{{Mount: "/full", ID: "1"}, {Mount: "/new", ID: "2"}}

	p.Probe([]Mount{{"1", "/full"}})
	p.Apply(fs)
	out, err := json.Marshal(fs)
	require.NoError(t, err)

	assert.JSONEq(t, `[
		{"mount": "/full", "source": "", "type": "", "usage": {"size_bytes": 4096000, "used_bytes": 4096000, "avail_bytes": 0}},
		{"mount": "/new", "source": "", "type": "", "error": "not read yet"}
	]`, string(out))
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
	mounts := []Mount{{"1", "/"}, {"2", "/hung"}}
	p.Probe(mounts)
	hang = true

	start := time.Now()
	p.Probe(mounts)
	p.Probe(mounts)
	assert.Less(t, time.Since(start), time.Second, "Probe returns at its deadline")
	assert.Equal(t, int32(1), hungCalls.Load(), "a mount still being read is not read again")

	fs := []clusterv1.FilesystemFacts{{Mount: "/", ID: "1"}, {Mount: "/hung", ID: "2"}}
	p.Apply(fs)
	assert.Empty(t, fs[0].Error)
	assert.Contains(t, fs[1].Error, "not answering; sizes as of ")
	require.NotNil(t, fs[1].Usage, "the last good reading is kept")
	assert.Equal(t, uint64(1000*4096), fs[1].Usage.SizeBytes)

	close(release)
	require.Eventually(t, func() bool {
		fs := []clusterv1.FilesystemFacts{{Mount: "/hung", ID: "2"}}
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

	p.Probe([]Mount{{"1", "/hung"}})

	fs := []clusterv1.FilesystemFacts{{Mount: "/hung", ID: "1"}}
	p.Apply(fs)
	assert.Equal(t, clusterv1.FilesystemFacts{Mount: "/hung", ID: "1", Error: "not answering"}, fs[0])
}

func TestUsageReadsAFilesystemReplacedAtTheSamePathAfresh(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	p := &UsageProber{Deadline: 20 * time.Millisecond, Statfs: func(path string, st *unix.Statfs_t) error {
		// The old filesystem answers once, then hangs; its replacement answers.
		if n := calls.Add(1); n == 2 {
			<-release
		} else if n > 2 {
			st.Frsize, st.Blocks, st.Bfree, st.Bavail = 4096, 10, 10, 10
			return nil
		}
		return fakeStatfs(path, st)
	}}
	p.Probe([]Mount{{"40 8:1 / /data", "/data"}})
	p.Probe([]Mount{{"40 8:1 / /data", "/data"}})

	replaced := []clusterv1.FilesystemFacts{{Mount: "/data", ID: "41 8:17 / /data"}}
	p.Apply(replaced)
	assert.Equal(t, "not read yet", replaced[0].Error, "the old filesystem's figures are not inherited")

	p.Probe([]Mount{{"41 8:17 / /data", "/data"}})
	replaced[0].Error = ""
	p.Apply(replaced)
	assert.Empty(t, replaced[0].Error, "a stuck read of the old filesystem does not block the new one")
	assert.Equal(t, &clusterv1.FilesystemUsage{SizeBytes: 10 * 4096, AvailBytes: 10 * 4096}, replaced[0].Usage)
}
