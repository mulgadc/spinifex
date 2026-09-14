package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/minio/highwayhash"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"
)

var kvCmd = &cobra.Command{
	Use:   "kv",
	Short: "Inspect JetStream KV state",
}

var kvDigestCmd = &cobra.Command{
	Use:   "digest",
	Short: "Print a digest of every stream held by this node's JetStream replica",
	Long: `Read this node's own copy of every JetStream stream and print one line per
stream: name, first and last sequence, message count, and a SHA-256 over every
stored message (sequence, subject, timestamp, headers, data).

Run it on every node and compare the output. Replicas of one stream must
produce identical lines; any difference means the replicas have diverged, and
--seqs prints a hash per sequence to find where. For a check that tells a
diverged replica from one that is merely behind, run it with --json --seqs on
every node and pass the results to "spx admin kv compare".

It never touches the live store. A direct get over NATS may be answered by any
replica, so instead the streams tree is copied to --work-dir and read by a
private in-process server with no listener, which is removed afterwards. The
copy's stream config is set to one replica with no expiry so it can be loaded.

Writes landing during the copy, or applied but not yet flushed to disk, can
make a replica's tail differ, so gate API writes and re-run before calling a
tail difference divergence.`,
	Run: runKVDigest,
}

func init() {
	adminCmd.AddCommand(kvCmd)
	kvCmd.AddCommand(kvDigestCmd)
	kvDigestCmd.Flags().String("store-dir", "", "JetStream store to read (default <spinifex-dir>/nats/jetstream)")
	kvDigestCmd.Flags().String("work-dir", os.TempDir(), "Directory to hold the temporary copy")
	kvDigestCmd.Flags().StringSlice("stream", nil, "Only these streams (repeatable)")
	kvDigestCmd.Flags().Bool("seqs", false, "Also print a hash per sequence")
	kvDigestCmd.Flags().Bool("json", false, "Print one JSON document, the input to \"spx admin kv compare\"")
}

// nodeDigest is one node's digest report, the unit "kv compare" reads.
type nodeDigest struct {
	Host     string         `json:"host"`
	Store    string         `json:"store"`
	CopiedAt time.Time      `json:"copied_at"`
	Skipped  []string       `json:"skipped,omitempty"`
	Streams  []streamDigest `json:"streams"`
}

// streamDigest is one stream's content as held by one replica.
type streamDigest struct {
	streamMeta

	Name     string      `json:"name"`
	FirstSeq uint64      `json:"first_seq"`
	LastSeq  uint64      `json:"last_seq"`
	Msgs     uint64      `json:"msgs"`
	Digest   string      `json:"sha256"`
	Seqs     []seqDigest `json:"seqs,omitempty"`
}

// streamMeta is the part of the live stream config that decides which replica
// differences are legitimate. It is read before the copy is rewritten.
type streamMeta struct {
	Replicas    int           `json:"replicas,omitempty"`
	Retention   string        `json:"retention,omitempty"`
	MaxAge      time.Duration `json:"max_age_ns,omitempty"`
	AllowMsgTTL bool          `json:"allow_msg_ttl,omitempty"`
	MaxMsgs     int64         `json:"max_msgs,omitempty"`
	MaxBytes    int64         `json:"max_bytes,omitempty"`
}

// seqDigest is the hash of one stored sequence, or "deleted" when the replica
// has no message at that sequence. Subject, time and TTL are empty then.
type seqDigest struct {
	Seq     uint64        `json:"seq"`
	Hash    string        `json:"hash"`
	Subject string        `json:"subject,omitempty"`
	Time    time.Time     `json:"time,omitzero"`
	TTL     time.Duration `json:"ttl_ns,omitempty"`
}

// deletedHash marks a sequence the replica holds no message for.
const deletedHash = "deleted"

func runKVDigest(cmd *cobra.Command, _ []string) {
	storeDir, _ := cmd.Flags().GetString("store-dir")
	if storeDir == "" {
		spxRoot, _ := cmd.Flags().GetString("spinifex-dir")
		if spxRoot == "" {
			spxRoot = DefaultDataDir()
		}
		storeDir = jetStreamStoreDir(spxRoot)
	}
	workParent, _ := cmd.Flags().GetString("work-dir")
	only, _ := cmd.Flags().GetStringSlice("stream")
	withSeqs, _ := cmd.Flags().GetBool("seqs")
	asJSON, _ := cmd.Flags().GetBool("json")

	work, err := os.MkdirTemp(workParent, "spx-kv-digest-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "kv digest: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(work)

	copiedAt := time.Now().UTC()
	skipped, metas, err := copyStreamTree(storeDir, filepath.Join(work, "jetstream"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "kv digest: %v\n", err)
		os.RemoveAll(work)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
	defer cancel()
	digests, err := digestStore(ctx, work, only, withSeqs, metas)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kv digest: %v\n", err)
		cancel()
		os.RemoveAll(work)
		os.Exit(1)
	}

	host, _ := os.Hostname()
	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		if err := enc.Encode(nodeDigest{Host: host, Store: storeDir, CopiedAt: copiedAt, Skipped: skipped, Streams: digests}); err != nil {
			fmt.Fprintf(os.Stderr, "kv digest: %v\n", err)
			os.Exit(1)
		}
		return
	}
	writeDigests(cmd.OutOrStdout(), host, storeDir, copiedAt, skipped, digests)
}

// writeDigests prints the report. Only the "#" lines carry node-specific
// values, so `grep -v '^#'` output from two replicas diffs cleanly.
func writeDigests(w io.Writer, host, storeDir string, copiedAt time.Time, skipped []string, digests []streamDigest) {
	fmt.Fprintf(w, "# host=%s store=%s copied_at=%s\n", host, storeDir, copiedAt.Format(time.RFC3339))
	for _, s := range skipped {
		fmt.Fprintf(w, "# skipped account stream (not read): %s\n", s)
	}
	for _, d := range digests {
		fmt.Fprintf(w, "%s\tfirst=%d\tlast=%d\tmsgs=%d\tsha256=%s\n", d.Name, d.FirstSeq, d.LastSeq, d.Msgs, d.Digest)
		for _, s := range d.Seqs {
			fmt.Fprintf(w, "%s\tseq=%d\t%s\n", d.Name, s.Seq, s.Hash)
		}
	}
}

// globalAccountDir is the account Spinifex's token-authenticated clients use.
// Streams under any other account are listed but not read.
const globalAccountDir = "$G"

// copyStreamTree copies <storeDir>/$G/streams to <dst>/$G/streams, leaving out
// consumer state (obs). It returns streams found under other accounts and each
// copied stream's config as the live server held it, keyed by stream name.
func copyStreamTree(storeDir, dst string) (skipped []string, metas map[string]streamMeta, err error) {
	streams, err := localStreams(storeDir)
	if err != nil {
		return nil, nil, err
	}
	if len(streams) == 0 {
		return nil, nil, fmt.Errorf("no streams found under %s", storeDir)
	}
	for _, s := range streams {
		if !strings.HasPrefix(s, globalAccountDir+"/") {
			skipped = append(skipped, s)
		}
	}

	src := filepath.Join(storeDir, globalAccountDir, "streams")
	out := filepath.Join(dst, globalAccountDir, "streams")
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		// <stream>/obs holds consumers, which are not stream content.
		if parts := strings.Split(rel, string(filepath.Separator)); d.IsDir() && len(parts) == 2 && parts[1] == "obs" {
			return filepath.SkipDir
		}
		target := filepath.Join(out, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("copy streams from %s: %w", src, err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		return nil, nil, err
	}
	metas = make(map[string]streamMeta, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			meta, err := rewriteStreamMetaStandalone(filepath.Join(out, e.Name()))
			if err != nil {
				return nil, nil, err
			}
			metas[e.Name()] = meta
		}
	}
	return skipped, metas, nil
}

// rewriteStreamMetaStandalone makes a copied stream loadable by a standalone
// server, which refuses replicas > 1 and would expire aged messages on
// recovery. meta.sum is highwayhash64 keyed by sha256 of the directory name.
// It returns the settings as they were before the rewrite.
func rewriteStreamMetaStandalone(streamDir string) (streamMeta, error) {
	metaPath := filepath.Join(streamDir, "meta.inf")
	raw, err := os.ReadFile(metaPath)
	if errors.Is(err, os.ErrNotExist) {
		return streamMeta{}, nil
	}
	if err != nil {
		return streamMeta{}, err
	}
	var orig struct {
		Replicas    int           `json:"num_replicas"`
		Retention   string        `json:"retention"`
		MaxAge      time.Duration `json:"max_age"`
		AllowMsgTTL bool          `json:"allow_msg_ttl"`
		MaxMsgs     int64         `json:"max_msgs"`
		MaxBytes    int64         `json:"max_bytes"`
	}
	if err := json.Unmarshal(raw, &orig); err != nil {
		return streamMeta{}, fmt.Errorf("parse %s: %w", metaPath, err)
	}
	kept := streamMeta{
		Replicas: orig.Replicas, Retention: orig.Retention, MaxAge: orig.MaxAge,
		AllowMsgTTL: orig.AllowMsgTTL, MaxMsgs: orig.MaxMsgs, MaxBytes: orig.MaxBytes,
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var meta map[string]any
	if err := dec.Decode(&meta); err != nil {
		return streamMeta{}, fmt.Errorf("parse %s: %w", metaPath, err)
	}
	meta["num_replicas"] = 1
	meta["max_age"] = 0
	delete(meta, "allow_msg_ttl")
	delete(meta, "subject_delete_marker_ttl")
	delete(meta, "placement")
	out, err := json.Marshal(meta)
	if err != nil {
		return streamMeta{}, err
	}
	key := sha256.Sum256([]byte(filepath.Base(streamDir)))
	hh, err := highwayhash.New64(key[:])
	if err != nil {
		return streamMeta{}, err
	}
	hh.Write(out)
	if err := os.WriteFile(metaPath, out, 0o600); err != nil {
		return streamMeta{}, err
	}
	return kept, os.WriteFile(filepath.Join(streamDir, "meta.sum"), []byte(hex.EncodeToString(hh.Sum(nil))), 0o600)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// digestStore opens storeParent (holding jetstream/) in a standalone server
// that accepts in-process connections only, and digests each stream.
func digestStore(ctx context.Context, storeParent string, only []string, withSeqs bool, metas map[string]streamMeta) ([]streamDigest, error) {
	ns, err := server.NewServer(&server.Options{
		ServerName: "spx-kv-digest",
		JetStream:  true,
		StoreDir:   storeParent,
		DontListen: true,
		NoLog:      true,
		NoSigs:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("create reader server: %w", err)
	}
	ns.Start()
	defer func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	}()
	if !ns.ReadyForConnections(30 * time.Second) {
		return nil, errors.New("reader server did not become ready")
	}

	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		return nil, fmt.Errorf("connect to reader server: %w", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, err
	}

	var names []string
	lister := js.StreamNames(ctx)
	for name := range lister.Name() {
		if len(only) == 0 || slices.Contains(only, name) {
			names = append(names, name)
		}
	}
	if err := lister.Err(); err != nil {
		return nil, fmt.Errorf("list streams: %w", err)
	}
	sort.Strings(names)

	digests := make([]streamDigest, 0, len(names))
	for _, name := range names {
		d, err := digestStream(ctx, js, name, withSeqs)
		if err != nil {
			return nil, err
		}
		d.streamMeta = metas[name]
		digests = append(digests, d)
	}
	return digests, nil
}

func digestStream(ctx context.Context, js jetstream.JetStream, name string, withSeqs bool) (streamDigest, error) {
	s, err := js.Stream(ctx, name)
	if err != nil {
		return streamDigest{}, fmt.Errorf("open stream %s: %w", name, err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		return streamDigest{}, fmt.Errorf("stream info %s: %w", name, err)
	}
	st := info.State
	d := streamDigest{Name: name, FirstSeq: st.FirstSeq, LastSeq: st.LastSeq, Msgs: st.Msgs}

	whole := sha256.New()
	for seq := st.FirstSeq; st.Msgs > 0 && seq <= st.LastSeq; seq++ {
		msg, err := s.GetMsg(ctx, seq)
		sd := seqDigest{Seq: seq}
		switch {
		case errors.Is(err, jetstream.ErrMsgNotFound):
			sd.Hash = deletedHash
		case err != nil:
			return streamDigest{}, fmt.Errorf("read %s seq %d: %w", name, seq, err)
		default:
			sd.Hash = hashStoredMsg(msg)
			sd.Subject = msg.Subject
			sd.Time = msg.Time.UTC()
			sd.TTL = msgTTL(msg.Header.Get(jetstream.MsgTTLHeader))
		}
		fmt.Fprintf(whole, "%d:%s\n", seq, sd.Hash)
		if withSeqs {
			d.Seqs = append(d.Seqs, sd)
		}
	}
	d.Digest = hex.EncodeToString(whole.Sum(nil))
	return d, nil
}

// hashStoredMsg hashes everything a replica stores for one sequence. Header
// keys are sorted so the hash does not depend on map iteration order.
func hashStoredMsg(m *jetstream.RawStreamMsg) string {
	h := sha256.New()
	var n [8]byte
	field := func(b []byte) {
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	binary.BigEndian.PutUint64(n[:], m.Sequence)
	h.Write(n[:])
	field([]byte(m.Subject))
	binary.BigEndian.PutUint64(n[:], uint64(m.Time.UnixNano()))
	h.Write(n[:])
	keys := make([]string, 0, len(m.Header))
	for k := range m.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		field([]byte(k))
		for _, v := range m.Header[k] {
			field([]byte(v))
		}
	}
	field(m.Data)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// msgTTL parses a Nats-TTL header: a Go duration or whole seconds. "never", an
// absent header or an unparseable one yield zero, meaning no expiry.
func msgTTL(v string) time.Duration {
	if v == "" {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}
