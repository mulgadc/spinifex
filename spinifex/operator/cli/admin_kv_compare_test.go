package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/operator/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

type msg struct {
	seq     uint64
	subject string
	hash    string // "deleted" for a gap
	at      time.Time
}

type stream struct {
	name     string
	replicas int
	maxAge   time.Duration
	maxMsgs  int64
	msgs     []msg
	noSeqs   bool
}

// doc renders one node's digest the way "kv digest --json --seqs" prints it.
func doc(t *testing.T, host string, copiedAt time.Time, streams ...stream) string {
	t.Helper()
	var out []map[string]any
	for _, s := range streams {
		var seqs []map[string]any
		var first, last, count uint64
		digest := host + ":" // only equality matters to compare
		for _, m := range s.msgs {
			if first == 0 {
				first = m.seq
			}
			last = m.seq
			e := map[string]any{"seq": m.seq, "hash": m.hash}
			if m.hash != "deleted" {
				count++
				e["subject"] = m.subject
				at := m.at
				if at.IsZero() {
					at = t0
				}
				e["time"] = at
			}
			digest += m.hash + ","
			seqs = append(seqs, e)
		}
		sd := map[string]any{"name": s.name, "first_seq": first, "last_seq": last, "msgs": count, "sha256": digest}
		if s.replicas > 0 {
			sd["replicas"] = s.replicas
		}
		if s.maxAge > 0 {
			sd["max_age_ns"] = s.maxAge
		}
		if s.maxMsgs > 0 {
			sd["max_msgs"] = s.maxMsgs
		}
		if !s.noSeqs {
			sd["seqs"] = seqs
		}
		out = append(out, sd)
	}
	raw, err := json.Marshal(map[string]any{"host": host, "store": "/s", "copied_at": copiedAt, "streams": out})
	require.NoError(t, err)
	return string(raw)
}

func kvs(msgs ...msg) []msg { return msgs }

func TestKVCompareIdentical(t *testing.T) {
	s := stream{name: "KV_a", replicas: 3, msgs: kvs(msg{1, "k", "h1", t0}, msg{2, "k", "h2", t0})}
	// The digest field embeds the host, so this exercises the per-seq path too.
	divergent, report, err := cli.CompareKVDigests(doc(t, "n1", t0, s), doc(t, "n2", t0, s), doc(t, "n3", t0, s))
	require.NoError(t, err)
	assert.False(t, divergent, report)
	assert.Contains(t, report, "1 streams: 1 consistent, 0 diverged")
}

// The prod failure: each node wrote its own copy before formation, so the same
// sequence holds different content on different replicas.
func TestKVCompareSameSeqDifferentContent(t *testing.T) {
	a := stream{name: "KV_acct", replicas: 2, msgs: kvs(msg{1, "$KV.acct.000000000002", "aaaa", t0})}
	b := stream{name: "KV_acct", replicas: 2, msgs: kvs(msg{1, "$KV.acct.000000000002", "bbbb", t0})}
	divergent, report, err := cli.CompareKVDigests(doc(t, "node1", t0, a), doc(t, "node2", t0, b))
	require.NoError(t, err)
	assert.True(t, divergent)
	assert.Contains(t, report, "DIVERGED  KV_acct")
	assert.Contains(t, report, "seq 1 $KV.acct.000000000002 holds different content: node1=aaaa node2=bbbb")
}

func TestKVCompareMissingMessage(t *testing.T) {
	full := kvs(msg{1, "k", "h1", t0}, msg{2, "j", "h2", t0}, msg{3, "k", "h3", t0})

	t.Run("superseded by a later write to the subject", func(t *testing.T) {
		lagging := stream{name: "KV_a", replicas: 2, msgs: full}
		purged := stream{name: "KV_a", replicas: 2, msgs: kvs(msg{1, "", "deleted", time.Time{}}, full[1], full[2])}
		divergent, report, err := cli.CompareKVDigests(doc(t, "n1", t0, lagging), doc(t, "n2", t0, purged))
		require.NoError(t, err)
		assert.False(t, divergent, report)
		assert.Contains(t, report, "1 superseded or expired")
	})

	t.Run("nothing explains it", func(t *testing.T) {
		have := stream{name: "KV_a", replicas: 2, msgs: full}
		lost := stream{name: "KV_a", replicas: 2, msgs: kvs(full[0], msg{2, "", "deleted", time.Time{}}, full[2])}
		divergent, report, err := cli.CompareKVDigests(doc(t, "n1", t0, have), doc(t, "n2", t0, lost))
		require.NoError(t, err)
		assert.True(t, divergent)
		assert.Contains(t, report, "seq 2 j is on n1 but missing from n2")
	})

	t.Run("aged out under max_age before the replica was copied", func(t *testing.T) {
		old := t0.Add(-time.Hour)
		have := stream{name: "KV_lease", replicas: 2, maxAge: time.Minute, msgs: kvs(msg{1, "l", "h1", old}, msg{2, "m", "h2", t0})}
		gone := stream{name: "KV_lease", replicas: 2, maxAge: time.Minute, msgs: kvs(msg{2, "m", "h2", t0})}
		divergent, report, err := cli.CompareKVDigests(doc(t, "n1", t0, have), doc(t, "n2", t0, gone))
		require.NoError(t, err)
		assert.False(t, divergent, report)

		have.maxAge, gone.maxAge = 0, 0
		divergent, _, err = cli.CompareKVDigests(doc(t, "n1", t0, have), doc(t, "n2", t0, gone))
		require.NoError(t, err)
		assert.True(t, divergent, "without max_age the same gap is unexplained")
	})

	t.Run("head dropped by a stream limit", func(t *testing.T) {
		behind := stream{name: "S", replicas: 2, maxMsgs: 2, msgs: kvs(msg{1, "a", "h1", t0}, msg{2, "b", "h2", t0})}
		ahead := stream{name: "S", replicas: 2, maxMsgs: 2, msgs: kvs(msg{2, "b", "h2", t0}, msg{3, "c", "h3", t0})}
		divergent, report, err := cli.CompareKVDigests(doc(t, "n1", t0, behind), doc(t, "n2", t0, ahead))
		require.NoError(t, err)
		assert.False(t, divergent, report)
	})
}

func TestKVCompareTailLag(t *testing.T) {
	behind := stream{name: "KV_a", replicas: 2, msgs: kvs(msg{1, "k", "h1", t0}, msg{2, "k", "h2", t0})}
	ahead := stream{name: "KV_a", replicas: 2, msgs: kvs(msg{1, "k", "h1", t0}, msg{2, "k", "h2", t0}, msg{3, "k", "h3", t0}, msg{4, "k", "h4", t0})}
	divergent, report, err := cli.CompareKVDigests(doc(t, "n1", t0, behind), doc(t, "n2", t0, ahead))
	require.NoError(t, err)
	assert.False(t, divergent, report)
	assert.Contains(t, report, "tail not compared: n2 +2")
}

func TestKVCompareNoCommonRange(t *testing.T) {
	a := stream{name: "KV_a", replicas: 2, msgs: kvs(msg{1, "k", "h1", t0})}
	b := stream{name: "KV_a", replicas: 2, msgs: kvs(msg{5, "k", "h5", t0})}
	divergent, report, err := cli.CompareKVDigests(doc(t, "n1", t0, a), doc(t, "n2", t0, b))
	require.NoError(t, err)
	assert.True(t, divergent)
	assert.Contains(t, report, "no sequence range is common")
}

// A 60s lease bucket renewed every few seconds turns over between digests
// taken a couple of seconds apart, so its replicas share no window.
func TestKVCompareVolatileStream(t *testing.T) {
	t1, t2, t3 := t0, t0.Add(2*time.Second), t0.Add(4*time.Second)
	lease := func(maxAge time.Duration, msgs ...msg) stream {
		return stream{name: "KV_bedrock-leader", replicas: 3, maxAge: maxAge, msgs: msgs}
	}
	early := kvs(msg{3984, "$KV.bedrock-leader.leader", "h3984", t1.Add(-time.Second)})
	late := kvs(msg{3985, "$KV.bedrock-leader.leader", "h3985", t2.Add(-time.Second)}, msg{3986, "$KV.bedrock-leader.leader", "h3986", t2})

	t.Run("adjacent windows of a max_age stream are skipped, not diverged", func(t *testing.T) {
		divergent, report, err := cli.CompareKVDigests(
			doc(t, "node1", t1, lease(time.Minute, early...)),
			doc(t, "node2", t2, lease(time.Minute, late...)),
			doc(t, "node3", t3, lease(time.Minute, late...)))
		require.NoError(t, err)
		assert.False(t, divergent, report)
		assert.Contains(t, report, "skipped   KV_bedrock-leader")
		assert.Contains(t, report, "volatile: max_age 1m0s")
		assert.Contains(t, report, "1 streams: 0 consistent, 0 diverged, 1 skipped as volatile")
	})

	t.Run("a sequence two replicas share must still match", func(t *testing.T) {
		mid := kvs(msg{3984, "$KV.bedrock-leader.leader", "other", t1.Add(-time.Second)}, late[0])
		divergent, report, err := cli.CompareKVDigests(
			doc(t, "node1", t1, lease(time.Minute, early...)),
			doc(t, "node2", t2, lease(time.Minute, mid...)),
			doc(t, "node3", t3, lease(time.Minute, late[1])))
		require.NoError(t, err)
		assert.True(t, divergent, report)
		assert.Contains(t, report, "seq 3984 $KV.bedrock-leader.leader holds different content: node1=h3984 node2=other")
	})

	t.Run("a long-lived bucket with no common window is still diverged", func(t *testing.T) {
		divergent, report, err := cli.CompareKVDigests(
			doc(t, "node1", t1, lease(0, early...)),
			doc(t, "node2", t2, lease(0, late...)),
			doc(t, "node3", t3, lease(0, late...)))
		require.NoError(t, err)
		assert.True(t, divergent, report)
		assert.Contains(t, report, "no sequence range is common")
	})

	t.Run("digests copied at the same instant are not excused", func(t *testing.T) {
		divergent, report, err := cli.CompareKVDigests(
			doc(t, "node1", t1, lease(time.Minute, early...)),
			doc(t, "node2", t1, lease(time.Minute, late...)),
			doc(t, "node3", t1, lease(time.Minute, late...)))
		require.NoError(t, err)
		assert.True(t, divergent, report)
		assert.Contains(t, report, "no sequence range is common")
	})
}

func TestKVCompareReplicaPlacement(t *testing.T) {
	s := stream{name: "KV_a", replicas: 3, msgs: kvs(msg{1, "k", "h1", t0})}
	other := stream{name: "KV_b", replicas: 1, msgs: kvs(msg{1, "k", "h1", t0})}

	t.Run("fewer holders than replicas", func(t *testing.T) {
		divergent, report, err := cli.CompareKVDigests(doc(t, "n1", t0, s), doc(t, "n2", t0, s), doc(t, "n3", t0, other))
		require.NoError(t, err)
		assert.True(t, divergent)
		assert.Contains(t, report, "held by 2 of 3 replicas; missing from n3")
	})

	t.Run("more holders than replicas", func(t *testing.T) {
		divergent, report, err := cli.CompareKVDigests(doc(t, "n1", t0, other), doc(t, "n2", t0, other))
		require.NoError(t, err)
		assert.True(t, divergent)
		assert.Contains(t, report, "held by 2 nodes but configured for 1 replicas")
	})

	t.Run("replica counts disagree", func(t *testing.T) {
		r1 := s
		r1.replicas = 1
		divergent, report, err := cli.CompareKVDigests(doc(t, "n1", t0, s), doc(t, "n2", t0, r1))
		require.NoError(t, err)
		assert.True(t, divergent)
		assert.Contains(t, report, "replicas disagree on the replica count")
	})

	t.Run("single replica stream on one node", func(t *testing.T) {
		divergent, report, err := cli.CompareKVDigests(doc(t, "n1", t0, other), doc(t, "n2", t0))
		require.NoError(t, err)
		assert.False(t, divergent, report)
	})
}

func TestKVCompareNeedsSeqs(t *testing.T) {
	a := stream{name: "KV_a", replicas: 2, noSeqs: true, msgs: kvs(msg{1, "k", "h1", t0})}
	b := stream{name: "KV_a", replicas: 2, noSeqs: true, msgs: kvs(msg{1, "k", "h2", t0})}
	divergent, report, err := cli.CompareKVDigests(doc(t, "n1", t0, a), doc(t, "n2", t0, b))
	require.NoError(t, err)
	assert.True(t, divergent)
	assert.Contains(t, report, "re-run digest with --seqs")
}

func TestKVCompareInput(t *testing.T) {
	s := stream{name: "KV_a", replicas: 2, msgs: kvs(msg{1, "k", "h1", t0})}

	// Several documents in one stream, as "-" reads them from stdin.
	divergent, _, err := cli.CompareKVDigests(doc(t, "n1", t0, s) + "\n" + doc(t, "n2", t0, s))
	require.NoError(t, err)
	assert.False(t, divergent)

	_, _, err = cli.CompareKVDigests(doc(t, "n1", t0, s))
	assert.ErrorContains(t, err, "at least two nodes")

	_, _, err = cli.CompareKVDigests(doc(t, "n1", t0, s), doc(t, "n1", t0, s))
	assert.ErrorContains(t, err, "two digests are from host n1")

	_, _, err = cli.CompareKVDigests(`KV_a	first=1	last=1`)
	assert.Error(t, err, "text digest output is rejected")

	_, _, err = cli.CompareKVDigests(`{"streams":[]}`)
	assert.ErrorContains(t, err, "no host")

	_, _, err = cli.CompareKVDigests("  ")
	assert.ErrorContains(t, err, "no digest documents")
}

func TestAdminInitDiscardsJetStreamByDefault(t *testing.T) {
	assert.Equal(t, "true", cli.AdminInitFlagDefault("discard-jetstream"))
	assert.True(t, strings.HasPrefix(cli.AdminInitFlagDefault("ipsec"), "true"))
}
