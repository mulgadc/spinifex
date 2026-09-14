package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var kvCompareCmd = &cobra.Command{
	Use:   "compare <digest.json>...",
	Short: "Compare the kv digests of every node and report replicas that have diverged",
	Long: `Read the "spx admin kv digest --json --seqs" output of every node in the
cluster, one file per node ("-" reads any number of them from stdin), and
check that every stream's replicas hold the same content.

A replica that is merely behind is not divergent, so only these fail:

  - the same sequence holds different content on two replicas;
  - a message held by one replica is missing from another, and nothing on
    that replica explains it: no later write to the same subject, no expiry
    under max_age or a message TTL, and no stream limit dropping its head;
  - a stream is held by fewer nodes than its replica count, or by more;
  - replicas disagree on the replica count;
  - no sequence range is common to every replica, so nothing can be checked.

Sequences past the slowest replica's last one are not compared. Digests are
taken from live stores a few seconds apart, so re-run before trusting a
failure that could be a write landing between two copies.

Exits 0 when every stream is consistent, 1 on divergence, 2 on bad input.`,
	Args: cobra.MinimumNArgs(1),
	Run:  runKVCompare,
}

func init() {
	kvCmd.AddCommand(kvCompareCmd)
}

func runKVCompare(cmd *cobra.Command, args []string) {
	var nodes []nodeDigest
	for _, arg := range args {
		var r io.Reader
		if arg == "-" {
			r = cmd.InOrStdin()
		} else {
			f, err := os.Open(arg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "kv compare: %v\n", err)
				os.Exit(2)
			}
			defer f.Close()
			r = f
		}
		got, err := loadNodeDigests(r, arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kv compare: %v\n", err)
			os.Exit(2)
		}
		nodes = append(nodes, got...)
	}
	res, err := compareNodeDigests(nodes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kv compare: %v\n", err)
		os.Exit(2)
	}
	writeCompareResult(cmd.OutOrStdout(), nodes, res)
	if res.divergent() {
		os.Exit(1)
	}
}

// loadNodeDigests reads every JSON digest document in r.
func loadNodeDigests(r io.Reader, source string) ([]nodeDigest, error) {
	dec := json.NewDecoder(r)
	var out []nodeDigest
	for {
		var d nodeDigest
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, len(out)+1, err)
		}
		if d.Host == "" {
			return nil, fmt.Errorf("%s: document %d has no host; is it \"kv digest --json\" output?", source, len(out)+1)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no digest documents", source)
	}
	return out, nil
}

// maxFindingsPerStream caps what one stream prints; the rest are counted.
const maxFindingsPerStream = 5

type streamVerdict struct {
	Name     string
	Holders  []string
	Problems []string
	Notes    []string
}

type kvCompareResult struct {
	Streams []streamVerdict
}

func (r kvCompareResult) divergent() bool {
	return slices.ContainsFunc(r.Streams, func(v streamVerdict) bool { return len(v.Problems) > 0 })
}

// replicaView is one node's copy of one stream, indexed for the per-sequence rules.
type replicaView struct {
	host          string
	copiedAt      time.Time
	d             *streamDigest
	bySeq         map[uint64]seqDigest
	lastBySubject map[string]uint64
}

func compareNodeDigests(nodes []nodeDigest) (kvCompareResult, error) {
	if len(nodes) < 2 {
		return kvCompareResult{}, fmt.Errorf("need the digests of at least two nodes, got %d", len(nodes))
	}
	hosts := make([]string, 0, len(nodes))
	byName := map[string][]*replicaView{}
	for i := range nodes {
		n := &nodes[i]
		if slices.Contains(hosts, n.Host) {
			return kvCompareResult{}, fmt.Errorf("two digests are from host %s", n.Host)
		}
		hosts = append(hosts, n.Host)
		for j := range n.Streams {
			d := &n.Streams[j]
			v := &replicaView{host: n.Host, copiedAt: n.CopiedAt, d: d,
				bySeq: make(map[uint64]seqDigest, len(d.Seqs)), lastBySubject: map[string]uint64{}}
			for _, s := range d.Seqs {
				v.bySeq[s.Seq] = s
				if s.Hash != deletedHash && s.Seq > v.lastBySubject[s.Subject] {
					v.lastBySubject[s.Subject] = s.Seq
				}
			}
			byName[d.Name] = append(byName[d.Name], v)
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	var res kvCompareResult
	for _, name := range names {
		res.Streams = append(res.Streams, compareStream(name, hosts, byName[name]))
	}
	return res, nil
}

func compareStream(name string, hosts []string, views []*replicaView) streamVerdict {
	v := streamVerdict{Name: name}
	for _, r := range views {
		v.Holders = append(v.Holders, r.host)
	}

	counts := map[int][]string{}
	replicas := 0
	for _, r := range views {
		if n := r.d.Replicas; n > 0 {
			counts[n] = append(counts[n], r.host)
			replicas = max(replicas, n)
		}
	}
	if len(counts) > 1 {
		var parts []string
		for n, hs := range counts {
			parts = append(parts, fmt.Sprintf("%d on %s", n, strings.Join(hs, ",")))
		}
		sort.Strings(parts)
		v.Problems = append(v.Problems, "replicas disagree on the replica count: "+strings.Join(parts, "; "))
	}
	switch {
	case replicas > 0 && len(views) < replicas:
		missing := slices.DeleteFunc(slices.Clone(hosts), func(h string) bool { return slices.Contains(v.Holders, h) })
		msg := fmt.Sprintf("held by %d of %d replicas; missing from %s", len(views), replicas, strings.Join(missing, ","))
		if len(hosts) < replicas {
			msg += fmt.Sprintf(" (only %d digests given)", len(hosts))
		}
		v.Problems = append(v.Problems, msg)
	case replicas > 0 && len(views) > replicas:
		v.Problems = append(v.Problems, fmt.Sprintf("held by %d nodes but configured for %d replicas; a node outside the stream's group still holds a copy", len(views), replicas))
	}
	if len(views) < 2 {
		v.Notes = append(v.Notes, "single copy, nothing to compare")
		return v
	}

	identical := true
	for _, r := range views[1:] {
		if r.d.Digest != views[0].d.Digest || r.d.FirstSeq != views[0].d.FirstSeq || r.d.LastSeq != views[0].d.LastSeq {
			identical = false
			break
		}
	}
	if identical {
		v.Notes = append(v.Notes, fmt.Sprintf("identical, seqs %d-%d", views[0].d.FirstSeq, views[0].d.LastSeq))
		return v
	}
	for _, r := range views {
		if r.d.Msgs > 0 && len(r.d.Seqs) == 0 {
			v.Problems = append(v.Problems, "contents differ and "+r.host+"'s digest has no per-sequence detail; re-run digest with --seqs")
			return v
		}
	}

	lo, hi := views[0].d.FirstSeq, views[0].d.LastSeq
	seqSet := map[uint64]struct{}{}
	for _, r := range views {
		lo, hi = max(lo, r.d.FirstSeq), min(hi, r.d.LastSeq)
		for s := range r.bySeq {
			seqSet[s] = struct{}{}
		}
	}
	if lo > hi {
		v.Problems = append(v.Problems, "no sequence range is common to every replica, so nothing could be compared")
		return v
	}
	seqs := make([]uint64, 0, len(seqSet))
	for s := range seqSet {
		if s <= hi {
			seqs = append(seqs, s)
		}
	}
	slices.Sort(seqs)

	var problems []string
	explained := 0
	for _, seq := range seqs {
		var present, absent []*replicaView
		for _, r := range views {
			if e, ok := r.bySeq[seq]; ok && e.Hash != deletedHash {
				present = append(present, r)
			} else {
				absent = append(absent, r)
			}
		}
		if len(present) == 0 {
			continue
		}
		byHash := map[string][]string{}
		for _, r := range present {
			h := r.bySeq[seq].Hash
			byHash[h] = append(byHash[h], r.host)
		}
		if len(byHash) > 1 {
			var parts []string
			for h, hs := range byHash {
				parts = append(parts, strings.Join(hs, ",")+"="+h)
			}
			sort.Strings(parts)
			problems = append(problems, fmt.Sprintf("seq %d %s holds different content: %s", seq, present[0].bySeq[seq].Subject, strings.Join(parts, " ")))
			continue
		}
		msg := present[0].bySeq[seq]
		for _, r := range absent {
			if absenceExplained(r, msg) {
				explained++
				continue
			}
			problems = append(problems, fmt.Sprintf("seq %d %s is on %s but missing from %s, with no later write to that subject, expiry or limit to explain it",
				seq, msg.Subject, hostsOf(present), r.host))
		}
	}
	if len(problems) > maxFindingsPerStream {
		problems = append(problems[:maxFindingsPerStream], fmt.Sprintf("... and %d more", len(problems)-maxFindingsPerStream))
	}
	v.Problems = append(v.Problems, problems...)

	note := fmt.Sprintf("seqs %d-%d compared", lo, hi)
	if explained > 0 {
		note += fmt.Sprintf(", %d superseded or expired messages missing from some replicas", explained)
	}
	var ahead []string
	for _, r := range views {
		if r.d.LastSeq > hi {
			ahead = append(ahead, fmt.Sprintf("%s +%d", r.host, r.d.LastSeq-hi))
		}
	}
	if len(ahead) > 0 {
		note += ", tail not compared: " + strings.Join(ahead, " ")
	}
	v.Notes = append(v.Notes, note)
	return v
}

// absenceExplained reports whether a replica lacking msg removed it legitimately:
// the subject was written again later, the message aged out before that replica
// was copied, or a stream limit dropped it from a head the replica has moved past.
func absenceExplained(r *replicaView, msg seqDigest) bool {
	if msg.Subject == "" {
		return false
	}
	if r.lastBySubject[msg.Subject] > msg.Seq {
		return true
	}
	meta := r.d.streamMeta
	if meta.Retention != "" && meta.Retention != "limits" {
		return true
	}
	if !msg.Time.IsZero() {
		if meta.MaxAge > 0 && msg.Time.Add(meta.MaxAge).Before(r.copiedAt) {
			return true
		}
		if msg.TTL > 0 && msg.Time.Add(msg.TTL).Before(r.copiedAt) {
			return true
		}
	}
	return msg.Seq < r.d.FirstSeq && (meta.MaxMsgs > 0 || meta.MaxBytes > 0)
}

func hostsOf(views []*replicaView) string {
	hs := make([]string, len(views))
	for i, r := range views {
		hs[i] = r.host
	}
	return strings.Join(hs, ",")
}

func writeCompareResult(w io.Writer, nodes []nodeDigest, res kvCompareResult) {
	fmt.Fprintf(w, "compared %d digests:", len(nodes))
	for _, n := range nodes {
		fmt.Fprintf(w, " %s@%s", n.Host, n.CopiedAt.Format(time.TimeOnly))
	}
	fmt.Fprintln(w)
	bad := 0
	for _, v := range res.Streams {
		status := "ok"
		if len(v.Problems) > 0 {
			status = "DIVERGED"
			bad++
		}
		fmt.Fprintf(w, "%-9s %s  [%s]\n", status, v.Name, strings.Join(v.Holders, ","))
		for _, p := range v.Problems {
			fmt.Fprintf(w, "    ! %s\n", p)
		}
		for _, n := range v.Notes {
			fmt.Fprintf(w, "      %s\n", n)
		}
	}
	fmt.Fprintf(w, "%d streams: %d consistent, %d diverged\n", len(res.Streams), len(res.Streams)-bad, bad)
}
