package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon/mgmtipstore"
)

const mgmtInventorySchema = 1

// mgmtCutoverLimits is printed with every result: what the tools cannot see
// or verify, so a pass is not read as proof.
const mgmtCutoverLimits = `Limits:
  - The inventory's attestations are the operator's statements and are not verified:
    launchers_quiesced (no old daemon or launcher can start a guest on that host),
    nats_sync_always (the NATS server actually running on that host uses
    jetstream sync_interval: always; a config file proves only the file), and
    stopped_attested (that guest is stopped and its old launchers are quiesced).
  - A host missing from both the inventory and the cluster configuration is invisible here.`

var mgmtIPAMCmd = &cobra.Command{
	Use:   "mgmt-ipam",
	Short: "Cut a cluster over to the durable management IP allocator",
}

var mgmtIPAMActivateCmd = &cobra.Command{
	Use:   "activate",
	Short: "Seed the durable management IP record from a quiesced cluster's inventory",
	Long: `Seed the durable management IP record once, with every daemon on every host
stopped, before any node runs with mgmt_ipam = "durable".

The seed is the union of the legacy allocation key, the durable instance
records, unsettled Lambda attempts and the operator's per-host inventory. A
missing legacy key is not proof of an empty subnet: every holder any source
names must appear in the inventory. A duplicate address, conflicting owner,
unaccounted holder, missing host or missing attestation writes nothing.

--form seeds an empty formed record, and only when no source names a holder.`,
	Run: runMgmtIPAMActivate,
}

var mgmtIPAMPrepareNodeCmd = &cobra.Command{
	Use:   "prepare-node",
	Short: "Write this node's launch records for guests the operator attests are stopped",
	Long: `Run on each host with its daemon stopped. For every guest the inventory lists
on this node as stopped with stopped_attested, write an exited launch record
so a later start can prove the previous launch stopped. Running guests are left
for the daemon to adopt when it identifies their process. A guest with neither
stays held until it is repaired.`,
	Run: runMgmtIPAMPrepareNode,
}

func init() {
	adminCmd.AddCommand(mgmtIPAMCmd)
	mgmtIPAMCmd.AddCommand(mgmtIPAMActivateCmd, mgmtIPAMPrepareNodeCmd)
	mgmtIPAMActivateCmd.Flags().String("inventory", "", "Operator inventory file (required)")
	mgmtIPAMActivateCmd.Flags().Bool("form", false, "Seed an empty formed record for a cluster with no address holder")
	mgmtIPAMActivateCmd.Flags().Duration("timeout", 30*time.Second, "Maximum time for the reads and the seed")
	mgmtIPAMPrepareNodeCmd.Flags().String("inventory", "", "Operator inventory file (required)")
	_ = mgmtIPAMActivateCmd.MarkFlagRequired("inventory")
	_ = mgmtIPAMPrepareNodeCmd.MarkFlagRequired("inventory")
}

// mgmtInventory is the operator's account of every host that may hold a
// management address.
type mgmtInventory struct {
	Schema int                 `json:"schema"`
	Subnet string              `json:"subnet"`
	Hosts  []mgmtInventoryHost `json:"hosts"`
}

// mgmtInventoryHost is one host's account. Each local list is required, so an
// omitted list is a missing inventory, not an empty one.
type mgmtInventoryHost struct {
	Node              string                 `json:"node"`
	LaunchersQuiesced bool                   `json:"launchers_quiesced"`
	NATSSyncAlways    bool                   `json:"nats_sync_always"`
	VMs               *[]mgmtInventoryVM     `json:"vms"`
	Lambda            *[]mgmtInventoryLambda `json:"lambda"`
	LambdaTaps        *[]string              `json:"lambda_taps"`
}

func (h *mgmtInventoryHost) missingLists() []string {
	var missing []string
	if h.VMs == nil {
		missing = append(missing, "vms")
	}
	if h.Lambda == nil {
		missing = append(missing, "lambda")
	}
	if h.LambdaTaps == nil {
		missing = append(missing, "lambda_taps")
	}
	return missing
}

func (h *mgmtInventoryHost) vms() []mgmtInventoryVM {
	if h.VMs == nil {
		return nil
	}
	return *h.VMs
}

func (h *mgmtInventoryHost) lambda() []mgmtInventoryLambda {
	if h.Lambda == nil {
		return nil
	}
	return *h.Lambda
}

func (h *mgmtInventoryHost) lambdaTaps() []string {
	if h.LambdaTaps == nil {
		return nil
	}
	return *h.LambdaTaps
}

type mgmtInventoryVM struct {
	InstanceID      string `json:"instance_id"`
	MgmtIP          string `json:"mgmt_ip"`
	State           string `json:"state"`
	StoppedAttested bool   `json:"stopped_attested,omitempty"`
}

type mgmtInventoryLambda struct {
	AttemptID string `json:"attempt_id"`
	MgmtIP    string `json:"mgmt_ip"`
	Tap       string `json:"tap"`
}

const (
	inventoryRunning = "running"
	inventoryStopped = "stopped"
)

// lambdaHolderID is the holder name the Lambda test link allocates under.
func lambdaHolderID(attemptID string) string { return "lambda-" + attemptID }

func readMgmtInventory(path string) (*mgmtInventory, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read inventory: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var inv mgmtInventory
	if err := dec.Decode(&inv); err != nil {
		return nil, fmt.Errorf("decode inventory: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode inventory: trailing data after the document")
	}
	if inv.Schema != mgmtInventorySchema {
		return nil, fmt.Errorf("inventory schema %d is not %d", inv.Schema, mgmtInventorySchema)
	}
	return &inv, nil
}

// mgmtHolder is one source's claim that an instance holds an address.
type mgmtHolder struct {
	source     string
	instanceID string
	ip         string
	node       string
}

// lambdaOwnerAttempt is a node's current attempt in the Lambda execution owner.
type lambdaOwnerAttempt struct {
	node      string
	attemptID string
	state     string
}

// mgmtCutoverEvidence is what the cluster's own stores say, read once.
type mgmtCutoverEvidence struct {
	legacyKey bool
	legacy    []mgmtHolder
	records   []mgmtHolder
	lambda    []lambdaOwnerAttempt
	absent    []string
}

// mgmtCutoverPlan is the checked seed, or the reasons there is none.
type mgmtCutoverPlan struct {
	prefix    netip.Prefix
	authority mgmtipstore.Authority
	entries   []mgmtipstore.Entry
	refusals  []string
	warnings  []string
}

func (p *mgmtCutoverPlan) refusef(format string, args ...any) {
	p.refusals = append(p.refusals, fmt.Sprintf(format, args...))
}

func (p *mgmtCutoverPlan) warnf(format string, args ...any) {
	p.warnings = append(p.warnings, fmt.Sprintf(format, args...))
}

// lambdaAddressSettled reports whether an attempt in state can no longer hold
// its address: it never started, or its stop was proven and resources freed.
func lambdaAddressSettled(state string) bool {
	switch state {
	case execution.StateWithdrawn, execution.StateReleased, execution.StateEnded:
		return true
	}
	return false
}

func gatherMgmtCutover(ctx context.Context, js jetstream.JetStream, subnet string) (*mgmtCutoverEvidence, error) {
	ev := &mgmtCutoverEvidence{}
	if err := gatherLegacyKey(ctx, js, subnet, ev); err != nil {
		return nil, err
	}
	if err := gatherInstanceRecords(ctx, js, ev); err != nil {
		return nil, err
	}
	if err := gatherLambdaOwners(ctx, js, ev); err != nil {
		return nil, err
	}
	return ev, nil
}

func gatherLegacyKey(ctx context.Context, js jetstream.JetStream, subnet string, ev *mgmtCutoverEvidence) error {
	_, err := js.KeyValue(ctx, daemon.ClusterStateBucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		ev.absent = append(ev.absent, daemon.ClusterStateBucket)
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", daemon.ClusterStateBucket, err)
	}
	raw, _, err := kvstore.LeaderValue(ctx, js, daemon.ClusterStateBucket, daemon.MgmtIPAMLegacyKey(subnet))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy allocation key: %w", err)
	}
	var rec daemon.MgmtIPRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("decode legacy allocation key: %w", err)
	}
	if rec.Subnet != subnet {
		return fmt.Errorf("legacy allocation key names subnet %q, not %q", rec.Subnet, subnet)
	}
	ev.legacyKey = true
	for _, e := range rec.Allocated {
		ev.legacy = append(ev.legacy, mgmtHolder{source: "legacy key", instanceID: e.InstanceID, ip: e.IP, node: e.Node})
	}
	return nil
}

func gatherInstanceRecords(ctx context.Context, js jetstream.JetStream, ev *mgmtCutoverEvidence) error {
	holders, err := daemon.InstanceMgmtIPs(ctx, js)
	if errors.Is(err, daemon.ErrInstanceStateAbsent) {
		ev.absent = append(ev.absent, "instance-state")
		return nil
	}
	if err != nil {
		return err
	}
	for _, h := range holders {
		ev.records = append(ev.records, mgmtHolder{source: "instance record", instanceID: h.InstanceID, ip: h.MgmtIP, node: h.Node})
	}
	return nil
}

func gatherLambdaOwners(ctx context.Context, js jetstream.JetStream, ev *mgmtCutoverEvidence) error {
	owner, err := execution.Attach(ctx, js)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		ev.absent = append(ev.absent, execution.KVBucket)
		return nil
	}
	if err != nil {
		return err
	}
	nodes, err := owner.Nodes(ctx)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		a, err := owner.LeaderCurrent(ctx, node)
		if errors.Is(err, execution.ErrNotCurrent) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read Lambda attempt on %s: %w", node, err)
		}
		ev.lambda = append(ev.lambda, lambdaOwnerAttempt{node: node, attemptID: a.Grant.AttemptID, state: a.State})
	}
	return nil
}

// checkLambdaTaps refuses a host whose Lambda-owned taps and Lambda attempts
// do not account for each other one to one.
func checkLambdaTaps(p *mgmtCutoverPlan, h *mgmtInventoryHost) {
	owner := map[string]string{}
	for _, l := range h.lambda() {
		if l.Tap == "" {
			continue
		}
		if want := execution.TapName(l.AttemptID); l.Tap != want {
			p.refusef("host %s: Lambda attempt %s names tap %s, but its tap is %s", h.Node, l.AttemptID, l.Tap, want)
			continue
		}
		if prev, ok := owner[l.Tap]; ok {
			p.refusef("host %s: tap %s is claimed by Lambda attempts %s and %s", h.Node, l.Tap, prev, l.AttemptID)
			continue
		}
		owner[l.Tap] = l.AttemptID
	}
	present := map[string]bool{}
	for _, tap := range h.lambdaTaps() {
		if present[tap] {
			p.refusef("host %s: tap %s is listed twice in lambda_taps", h.Node, tap)
			continue
		}
		present[tap] = true
		if _, ok := owner[tap]; !ok {
			p.refusef("host %s: Lambda tap %s belongs to no listed Lambda attempt", h.Node, tap)
		}
	}
	for tap, attempt := range owner {
		if !present[tap] {
			p.refusef("host %s: Lambda attempt %s names tap %s, which lambda_taps does not list", h.Node, attempt, tap)
		}
	}
}

func checkInventoryAddress(p *mgmtCutoverPlan, where, ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil || addr.String() != ip || !p.prefix.Contains(addr) {
		p.refusef("%s: address %q is not a canonical address in %s", where, ip, p.prefix)
		return false
	}
	return true
}

// planMgmtCutover checks the inventory against the stores and builds the seed.
// Every inventory holder, and every holder a store names, must agree on one
// address and node per instance and one instance per address.
func planMgmtCutover(inv *mgmtInventory, clusterNodes []string, ev *mgmtCutoverEvidence, form bool) *mgmtCutoverPlan {
	p := &mgmtCutoverPlan{authority: mgmtipstore.AuthorityMigrated}
	if form {
		p.authority = mgmtipstore.AuthorityFormed
	}
	prefix, err := netip.ParsePrefix(inv.Subnet)
	if err != nil || prefix.Bits() != 24 || prefix.Masked() != prefix || !prefix.Addr().Is4() {
		p.refusef("inventory subnet %q is not an IPv4 /24 network", inv.Subnet)
		return p
	}
	p.prefix = prefix

	hostNodes := map[string]bool{}
	accounted := map[string]mgmtHolder{}
	var inventoryHolders []mgmtHolder
	account := func(h mgmtHolder) {
		if prev, ok := accounted[h.instanceID]; ok {
			p.refusef("%s is listed twice in the inventory: %s on %s and %s on %s", h.instanceID, prev.ip, prev.node, h.ip, h.node)
			return
		}
		accounted[h.instanceID] = h
	}
	for _, h := range inv.Hosts {
		if h.Node == "" {
			p.refusef("inventory host with no node name")
			continue
		}
		if hostNodes[h.Node] {
			p.refusef("host %s is listed twice in the inventory", h.Node)
			continue
		}
		hostNodes[h.Node] = true
		if !h.LaunchersQuiesced {
			p.refusef("host %s: launchers_quiesced is not attested", h.Node)
		}
		if !h.NATSSyncAlways {
			p.refusef("host %s: nats_sync_always is not attested for its running NATS server", h.Node)
		}
		if missing := h.missingLists(); len(missing) > 0 {
			p.refusef("host %s: inventory omits %s; list [] where the host has none", h.Node, strings.Join(missing, ", "))
			continue
		}
		checkLambdaTaps(p, &h)
		for _, v := range h.vms() {
			where := fmt.Sprintf("host %s guest %q", h.Node, v.InstanceID)
			switch {
			case v.InstanceID == "":
				p.refusef("host %s: guest with no instance id", h.Node)
				continue
			case v.State != inventoryRunning && v.State != inventoryStopped:
				p.refusef("%s: state %q is not running or stopped", where, v.State)
				continue
			case v.State == inventoryRunning && v.StoppedAttested:
				p.refusef("%s: listed running but attested stopped", where)
				continue
			}
			if !checkInventoryAddress(p, where, v.MgmtIP) {
				continue
			}
			holder := mgmtHolder{source: "inventory " + v.State, instanceID: v.InstanceID, ip: v.MgmtIP, node: h.Node}
			account(holder)
			if v.State == inventoryRunning {
				inventoryHolders = append(inventoryHolders, holder)
			}
		}
		for _, l := range h.lambda() {
			where := fmt.Sprintf("host %s Lambda attempt %q", h.Node, l.AttemptID)
			if l.AttemptID == "" {
				p.refusef("host %s: Lambda attempt with no attempt id", h.Node)
				continue
			}
			if l.MgmtIP == "" {
				p.refusef("%s: no address is accounted for; settle the attempt before activation", where)
				continue
			}
			if !checkInventoryAddress(p, where, l.MgmtIP) {
				continue
			}
			holder := mgmtHolder{source: "inventory Lambda", instanceID: lambdaHolderID(l.AttemptID), ip: l.MgmtIP, node: h.Node}
			account(holder)
			inventoryHolders = append(inventoryHolders, holder)
		}
	}
	for _, n := range clusterNodes {
		if !hostNodes[n] {
			p.refusef("cluster node %s is missing from the inventory", n)
		}
	}

	stored := slices.Concat(ev.legacy, ev.records)
	for _, a := range ev.lambda {
		if lambdaAddressSettled(a.state) {
			continue
		}
		id := lambdaHolderID(a.attemptID)
		h, ok := accounted[id]
		switch {
		case !ok:
			p.refusef("Lambda attempt %s on %s is %s and its address is not in the inventory", a.attemptID, a.node, a.state)
		case h.node != a.node:
			p.refusef("Lambda attempt %s is owned on %s but listed on %s", a.attemptID, a.node, h.node)
		}
	}

	seed := map[string]mgmtHolder{}
	add := func(h mgmtHolder) {
		inv, ok := accounted[h.instanceID]
		switch {
		case !ok:
			p.refusef("%s names %s at %s on %q, which no inventory host lists", h.source, h.instanceID, h.ip, h.node)
			return
		case inv.ip != h.ip:
			p.refusef("%s names %s at %s but the inventory lists %s", h.source, h.instanceID, h.ip, inv.ip)
			return
		case h.node != "" && h.node != inv.node:
			p.refusef("%s names %s on %s but the inventory lists it on %s", h.source, h.instanceID, h.node, inv.node)
			return
		}
		seed[h.instanceID] = inv
	}
	for _, h := range stored {
		add(h)
	}
	for _, h := range inventoryHolders {
		add(h)
	}

	byIP := map[string]string{}
	for _, h := range seed {
		if other, ok := byIP[h.ip]; ok {
			p.refusef("address %s is held by both %s and %s", h.ip, min(other, h.instanceID), max(other, h.instanceID))
			continue
		}
		byIP[h.ip] = h.instanceID
	}
	for id, h := range accounted {
		if _, held := seed[id]; held {
			continue
		}
		if other, ok := byIP[h.ip]; ok {
			p.warnf("stopped guest %s on %s saved %s, which %s holds; its start will be refused until it is repaired", id, h.node, h.ip, other)
		}
	}

	switch {
	case form && (ev.legacyKey || len(seed) > 0 || len(accounted) > 0):
		p.refusef("--form is only for a cluster with no address holder; the stores or the inventory name one")
	case !form && len(seed) == 0:
		p.refusef("no source names an address holder; rerun with --form only if this is an empty first formation")
	}

	for _, h := range seed {
		p.entries = append(p.entries, mgmtipstore.Entry{IP: h.ip, InstanceID: h.instanceID, Node: h.node})
	}
	sort.Slice(p.entries, func(i, j int) bool {
		return netip.MustParseAddr(p.entries[i].IP).Less(netip.MustParseAddr(p.entries[j].IP))
	})
	sort.Strings(p.refusals)
	sort.Strings(p.warnings)
	return p
}

// applyMgmtCutover writes the seed. Formation refuses a record that already
// holds addresses rather than merging into it.
func applyMgmtCutover(ctx context.Context, js jetstream.JetStream, p *mgmtCutoverPlan) (mgmtipstore.Record, error) {
	store, err := mgmtipstore.Create(ctx, js, daemon.MgmtIPAMBucket, "Spinifex management IP allocations")
	if err != nil {
		return mgmtipstore.Record{}, fmt.Errorf("create %s: %w", daemon.MgmtIPAMBucket, err)
	}
	if p.authority == mgmtipstore.AuthorityFormed {
		snap, err := store.Load(ctx, p.prefix)
		switch {
		case err == nil && len(snap.Entries()) > 0:
			return mgmtipstore.Record{}, fmt.Errorf("--form refused: the durable record for %s already holds %d addresses", p.prefix, len(snap.Entries()))
		case err != nil && !errors.Is(err, mgmtipstore.ErrNotAuthoritative):
			return mgmtipstore.Record{}, err
		}
	}
	return store.Seed(ctx, p.prefix, p.authority, p.entries)
}

func printMgmtCutoverPlan(w io.Writer, ev *mgmtCutoverEvidence, p *mgmtCutoverPlan) {
	if ev != nil {
		fmt.Fprintf(w, "Sources: legacy key present=%t holders=%d, instance records with an address=%d, Lambda owner attempts=%d\n",
			ev.legacyKey, len(ev.legacy), len(ev.records), len(ev.lambda))
		for _, b := range ev.absent {
			fmt.Fprintf(w, "Bucket %s does not exist; it names no holder\n", b)
		}
	}
	for _, r := range p.refusals {
		fmt.Fprintf(w, "REFUSED: %s\n", r)
	}
	for _, warning := range p.warnings {
		fmt.Fprintf(w, "WARNING: %s\n", warning)
	}
	fmt.Fprintln(w, mgmtCutoverLimits)
}

func clusterNodeNames(cfg *config.ClusterConfig) []string {
	names := make([]string, 0, len(cfg.Nodes))
	for n := range cfg.Nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// activateMgmtIPAM reads, checks and seeds. It writes nothing unless every
// check passes.
func activateMgmtIPAM(ctx context.Context, w io.Writer, js jetstream.JetStream, inv *mgmtInventory, clusterNodes []string, form bool) error {
	ev, err := gatherMgmtCutover(ctx, js, inv.Subnet)
	if err != nil {
		return err
	}
	p := planMgmtCutover(inv, clusterNodes, ev, form)
	printMgmtCutoverPlan(w, ev, p)
	if len(p.refusals) > 0 {
		return fmt.Errorf("activation refused with %d findings; nothing was written", len(p.refusals))
	}
	rec, err := applyMgmtCutover(ctx, js, p)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Seeded %s in %s: authority %s, %d addresses\n", rec.Subnet, daemon.MgmtIPAMBucket, rec.Authority, len(rec.Entries))
	for _, e := range rec.Entries {
		fmt.Fprintf(w, "  %s  %s  %s\n", e.IP, e.InstanceID, e.Node)
	}
	return nil
}

func runMgmtIPAMActivate(cmd *cobra.Command, _ []string) {
	path, _ := cmd.Flags().GetString("inventory")
	form, _ := cmd.Flags().GetBool("form")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	inv, err := readMgmtInventory(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mgmt-ipam activate: %v\n", err)
		os.Exit(1)
	}
	cfg, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mgmt-ipam activate: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mgmt-ipam activate: jetstream: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	if err := activateMgmtIPAM(ctx, os.Stdout, js, inv, clusterNodeNames(cfg), form); err != nil {
		fmt.Fprintf(os.Stderr, "mgmt-ipam activate: %v\n", err)
		os.Exit(1)
	}
}

// prepareMgmtNode writes exited launch records for this node's attested
// stopped guests under the launch records' lock, so it fails while a daemon
// on this state directory is running.
func prepareMgmtNode(w io.Writer, inv *mgmtInventory, node, launchDir string) error {
	var host *mgmtInventoryHost
	for i := range inv.Hosts {
		if inv.Hosts[i].Node == node {
			host = &inv.Hosts[i]
			break
		}
	}
	if host == nil {
		return fmt.Errorf("node %s is not in the inventory", node)
	}
	if !host.LaunchersQuiesced {
		return fmt.Errorf("node %s: launchers_quiesced is not attested", node)
	}
	if missing := host.missingLists(); len(missing) > 0 {
		return fmt.Errorf("node %s: inventory omits %s; list [] where the host has none", node, strings.Join(missing, ", "))
	}
	lock, err := daemon.LockMgmtLaunchDir(launchDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	var failed int
	for _, v := range host.vms() {
		switch {
		case v.State == inventoryRunning:
			fmt.Fprintf(w, "%s: running; left for the daemon to adopt once it identifies the process\n", v.InstanceID)
		case v.State != inventoryStopped:
			fmt.Fprintf(w, "%s: REFUSED: state %q is not running or stopped\n", v.InstanceID, v.State)
			failed++
		case !v.StoppedAttested:
			fmt.Fprintf(w, "%s: HELD: stopped is not attested, so no record is written and its launch will refuse\n", v.InstanceID)
		default:
			written, err := vm.RecordStoppedMgmtLaunch(launchDir, node, v.InstanceID, v.MgmtIP)
			switch {
			case err != nil:
				fmt.Fprintf(w, "%s: REFUSED: %v\n", v.InstanceID, err)
				failed++
			case written:
				fmt.Fprintf(w, "%s: wrote an exited launch record for %s\n", v.InstanceID, v.MgmtIP)
			default:
				fmt.Fprintf(w, "%s: kept its existing launch record for %s\n", v.InstanceID, v.MgmtIP)
			}
		}
	}
	fmt.Fprintln(w, mgmtCutoverLimits)
	if failed > 0 {
		return fmt.Errorf("%d guests refused", failed)
	}
	return nil
}

func runMgmtIPAMPrepareNode(cmd *cobra.Command, _ []string) {
	path, _ := cmd.Flags().GetString("inventory")
	inv, err := readMgmtInventory(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mgmt-ipam prepare-node: %v\n", err)
		os.Exit(1)
	}
	cfgPath := viper.GetString("config")
	if cfgPath == "" {
		cfgPath = DefaultConfigFile()
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mgmt-ipam prepare-node: load config: %v\n", err)
		os.Exit(1)
	}
	node, ok := cfg.Nodes[cfg.Node]
	if !ok {
		fmt.Fprintf(os.Stderr, "mgmt-ipam prepare-node: node %q has no configuration\n", cfg.Node)
		os.Exit(1)
	}
	if err := prepareMgmtNode(os.Stdout, inv, cfg.Node, daemon.MgmtLaunchDir(node.DataDir)); err != nil {
		fmt.Fprintf(os.Stderr, "mgmt-ipam prepare-node: %v\n", err)
		os.Exit(1)
	}
}
