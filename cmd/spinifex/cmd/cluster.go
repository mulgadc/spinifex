package cmd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

// systemIsSystemRunning is overridable in tests so hostIsStopping does not
// need a real systemd. systemctl exits non-zero for every state other than
// "running", so callers must inspect the output, not just the error.
var systemIsSystemRunning = func() (string, error) {
	out, err := exec.Command("systemctl", "is-system-running").Output()
	return strings.TrimSpace(string(out)), err
}

// knownSystemStates are the documented systemctl is-system-running values.
var knownSystemStates = map[string]bool{
	"running": true, "degraded": true, "maintenance": true,
	"stopping": true, "initializing": true, "starting": true, "offline": true,
}

// hostIsStopping reports whether systemd is genuinely unwinding into
// shutdown.target (reboot/poweroff/halt/kexec), as opposed to a plain
// `systemctl restart/stop spinifex.target` where the host stays up.
func hostIsStopping() (bool, error) {
	out, err := systemIsSystemRunning()

	// systemctl did not run at all, so the state is genuinely unknown. Fail
	// toward draining: a skipped drain on a real shutdown hard-kills guests,
	// while a spurious drain only costs a graceful stop/relaunch cycle.
	if out == "" {
		return true, fmt.Errorf("systemctl is-system-running produced no output: %w", err)
	}

	// Unrecognized output is untrusted; an unknown state must never be
	// mistaken for shutdown.
	if !knownSystemStates[out] {
		return false, fmt.Errorf("systemctl is-system-running returned unrecognized state %q", out)
	}

	return out == "stopping", nil
}

// systemctlListJobs is overridable in tests so stackIsRestarting does not
// need a real systemd. Output is one line per pending job in the form
// "JOB UNIT TYPE STATE" (systemctl list-jobs --no-legend --no-pager).
var systemctlListJobs = func() (string, error) {
	out, err := exec.Command("systemctl", "list-jobs", "--no-legend", "--no-pager").Output()
	return string(out), err
}

// stackIsRestarting reports whether a pending systemd job shows the spinifex
// stack is already coming back: a queued START job for spinifex.target
// (its stop leg resolves instantly, so only the start job stays pending
// during `systemctl restart`), or a queued RESTART job for
// spinifex-shutdown.service itself (PartOf propagates target-to-member
// only, so a direct unit restart looks different). No pending jobs is a
// clean "not restarting"; output with no recognizable job line is an error.
func stackIsRestarting() (bool, error) {
	out, err := systemctlListJobs()
	if err != nil {
		return false, fmt.Errorf("systemctl list-jobs: %w", err)
	}

	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return false, nil
	}

	sawParseableLine := false
	for line := range strings.SplitSeq(trimmed, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		sawParseableLine = true
		unit, jobType := fields[1], fields[2]
		if unit == "spinifex.target" && jobType == "start" {
			return true, nil
		}
		if unit == "spinifex-shutdown.service" && jobType == "restart" {
			return true, nil
		}
	}
	if !sawParseableLine {
		return false, fmt.Errorf("systemctl list-jobs: no parseable job lines in output %q", trimmed)
	}
	return false, nil
}

// shouldDrainOnStop decides whether a spinifex.target stop should drain
// local guests. A real host shutdown always drains. Otherwise a plain
// target stop drains too, since nothing then guarantees the stack comes
// back; only a systemd job proving a restart is already queued skips it.
// Any step systemctl cannot answer fails toward draining: a spurious drain
// only costs a graceful stop and relaunch, a skipped one costs a guest's
// data path.
func shouldDrainOnStop() (drain bool, reason string) {
	stopping, err := hostIsStopping()
	if err != nil {
		return true, fmt.Sprintf("could not determine host shutdown state, draining: %v", err)
	}
	if stopping {
		return true, "host is shutting down (reboot/poweroff)"
	}

	restarting, err := stackIsRestarting()
	if err != nil {
		return true, fmt.Sprintf("could not determine whether the spinifex stack is restarting, draining: %v", err)
	}
	if restarting {
		return false, "a restart of the spinifex stack is already queued"
	}

	return true, "plain target stop with no restart queued"
}

// runClusterShutdown orchestrates a phased, coordinated shutdown of the cluster.
func runClusterShutdown(cmd *cobra.Command, args []string) {
	force, _ := cmd.Flags().GetBool("force")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	cfg, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()

	nodeCount := len(cfg.Nodes)
	phases := []string{"gate", "drain", "storage", "persist", "infra"}

	if dryRun {
		fmt.Println("Cluster Shutdown Plan (dry-run)")
		fmt.Printf("  Nodes: %d\n", nodeCount)
		for name, nodeCfg := range cfg.Nodes {
			fmt.Printf("    - %s (services: %s)\n", name, strings.Join(nodeCfg.GetServices(), ", "))
		}
		fmt.Printf("  Phases: %s\n", strings.Join(phases, " -> "))
		fmt.Printf("  Timeout per phase: %s\n", timeout)
		fmt.Printf("  Force: %v\n", force)
		fmt.Println("\nPhase details:")
		fmt.Println("  1. GATE    - Stop API gateway and UI, reject new work")
		fmt.Println("  2. DRAIN   - Gracefully stop all VMs, persist state")
		fmt.Println("  3. STORAGE - Stop viperblock, cleanup nbdkit")
		fmt.Println("  4. PERSIST - Stop predastore")
		fmt.Println("  5. INFRA   - Stop NATS, exit daemons")
		return
	}

	fmt.Printf("Starting coordinated cluster shutdown (%d nodes)\n", nodeCount)
	fmt.Printf("Phases: %s\n", strings.Join(phases, " -> "))
	fmt.Printf("Timeout per phase: %s\n\n", timeout)

	// Write cluster shutdown marker to KV
	jsm, err := daemon.NewJetStreamManager(nc, 1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create JetStream manager: %v\n", err)
	} else {
		if err := jsm.InitClusterStateBucket(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to init cluster state bucket: %v\n", err)
		} else {
			state := &daemon.ClusterShutdownState{
				Initiator:  cfg.Node,
				Phase:      "starting",
				Started:    time.Now().UTC().Format(time.RFC3339),
				Timeout:    timeout.String(),
				Force:      force,
				NodesTotal: nodeCount,
				NodesAcked: make(map[string]string),
			}
			if err := jsm.WriteClusterShutdown(state); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to write shutdown state: %v\n", err)
			}
		}
	}

	start := time.Now()

	// Execute phases sequentially (except INFRA which is fire-and-forget)
	for _, phase := range phases {
		topic := "spinifex.cluster.shutdown." + phase
		req := daemon.ShutdownRequest{
			Phase:   phase,
			Force:   force,
			Timeout: int(timeout.Seconds()),
		}

		reqData, err := json.Marshal(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error marshaling request: %v\n", err)
			os.Exit(1)
		}

		if phase == "infra" {
			// INFRA is fire-and-forget — NATS is going down, no ACKs possible
			fmt.Printf("[INFRA] Sending final shutdown to all nodes...\n")
			if err := nc.Publish(topic, reqData); err != nil {
				fmt.Fprintf(os.Stderr, "Error publishing infra shutdown: %v\n", err)
			}
			nc.Flush()
			// Wait briefly for messages to propagate
			time.Sleep(200 * time.Millisecond)
			fmt.Printf("[INFRA] Complete\n")
			break
		}

		// For DRAIN phase, subscribe to progress updates
		var progressSub *nats.Subscription
		if phase == "drain" {
			progressSub, err = nc.Subscribe("spinifex.cluster.shutdown.progress", func(msg *nats.Msg) {
				var progress daemon.ShutdownProgress
				if err := json.Unmarshal(msg.Data, &progress); err != nil {
					return
				}
				fmt.Printf("  [%s] %s: %d/%d VMs remaining\n", strings.ToUpper(phase), progress.Node, progress.Remaining, progress.Total)
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to subscribe to progress: %v\n", err)
			}
		}

		// Collect ACKs from all nodes
		phaseStart := time.Now()
		fmt.Printf("[%s] Sending to %d node(s)...\n", strings.ToUpper(phase), nodeCount)

		acks, err := collectShutdownACKs(nc, topic, reqData, nodeCount, "", timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] Error: %v\n", strings.ToUpper(phase), err)
			if !force {
				fmt.Fprintf(os.Stderr, "Aborting shutdown. Use --force to continue despite errors.\n")
				os.Exit(1)
			}
		}

		// Unsubscribe from progress
		if progressSub != nil {
			if err := progressSub.Unsubscribe(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to unsubscribe from progress: %v\n", err)
			}
		}

		// Print results
		for _, ack := range acks {
			if ack.Error != "" {
				fmt.Printf("  [%s] %s: ERROR - %s\n", strings.ToUpper(phase), ack.Node, ack.Error)
			} else if len(ack.Stopped) > 0 {
				fmt.Printf("  [%s] %s: stopped %s\n", strings.ToUpper(phase), ack.Node, strings.Join(ack.Stopped, ", "))
			} else {
				fmt.Printf("  [%s] %s: OK\n", strings.ToUpper(phase), ack.Node)
			}
		}

		// A node that answered with an error has not completed the phase. Counting
		// it as done would advance the run into STORAGE and PERSIST, tearing
		// storage out from under guests that never stopped.
		ackedCount, failed := summariseShutdownACKs(acks)
		if len(failed) > 0 && !force {
			fmt.Fprintf(os.Stderr, "[%s] %d node(s) failed the phase: %s. Use --force to continue.\n",
				strings.ToUpper(phase), len(failed), strings.Join(failed, ", "))
			os.Exit(1)
		}
		if ackedCount < nodeCount && !force {
			fmt.Fprintf(os.Stderr, "[%s] Only %d/%d nodes completed. Use --force to continue.\n",
				strings.ToUpper(phase), ackedCount, nodeCount)
			os.Exit(1)
		}

		fmt.Printf("[%s] Complete (%d/%d nodes, %s)\n\n", strings.ToUpper(phase), ackedCount, nodeCount, time.Since(phaseStart).Round(time.Millisecond))
	}

	fmt.Printf("Cluster shutdown complete (%s)\n", time.Since(start).Round(time.Millisecond))
}

// runNodeDrainLocal runs the GATE and DRAIN shutdown phases against the local
// node only, leaving STORAGE/PERSIST/INFRA to systemd's ordered unit teardown.
// It is the ExecStop of spinifex-shutdown.service: guests are powered down and
// their volumes unmounted while every service is still up, so the subsequent
// service stops never tear storage out from under a running guest.
func runNodeDrainLocal(cmd *cobra.Command, args []string) {
	local, _ := cmd.Flags().GetBool("local")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	unlessRestarting, _ := cmd.Flags().GetBool("unless-restarting")
	// --only-if-host-stopping is a deprecated alias for --unless-restarting;
	// OR it in so a mixed-version node/unit-file pairing during a partial
	// upgrade still gets a gate rather than silently always draining.
	deprecatedOnlyIfHostStopping, _ := cmd.Flags().GetBool("only-if-host-stopping")
	unlessRestarting = unlessRestarting || deprecatedOnlyIfHostStopping
	if !local {
		fmt.Fprintln(os.Stderr, "Error: node drain currently supports only --local")
		os.Exit(1)
	}

	// Unset (an operator running this by hand), the command drains
	// unconditionally. Set, it drains on a real host shutdown and on a plain
	// target stop, and skips only when a restart of the stack is already
	// queued.
	if unlessRestarting {
		drain, reason := shouldDrainOnStop()
		if !drain {
			fmt.Printf("Skipping guest drain: %s. Guests are left running for the stack to reattach to.\n", reason)
			warnIfGuestsLeftRunning()
			return
		}
	}

	cfg, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()

	node := cfg.Node
	fmt.Printf("Draining local node %q (gate -> drain), timeout %s/phase\n", node, timeout)

	for _, phase := range []string{"gate", "drain"} {
		topic := "spinifex.cluster.shutdown." + phase
		req := localDrainRequest(phase, node, timeout)
		reqData, err := json.Marshal(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error marshaling request: %v\n", err)
			os.Exit(1)
		}

		ack, err := collectLocalShutdownACK(nc, topic, reqData, node, timeout)
		if err != nil {
			// Non-zero marks spinifex-shutdown.service failed (visible via
			// `systemctl --failed`) without blocking the target's teardown;
			// exiting 0 would hide guests left running as storage vanishes.
			slog.Error("drain phase failed: no ACK from local daemon before timeout; guests may be left running while storage is about to be torn down",
				"phase", phase, "node", node, "timeout", timeout, "error", err)
			reportGuestsLeftRunning(true, fmt.Sprintf("[%s] drain failed: guests left running with storage about to be torn down", strings.ToUpper(phase)))
			os.Exit(1)
		}
		if ack.Error != "" {
			fmt.Fprintf(os.Stderr, "[%s] %s: ERROR - %s\n", strings.ToUpper(phase), ack.Node, ack.Error)
			os.Exit(1)
		}
		if len(ack.Stopped) > 0 {
			fmt.Printf("[%s] %s: stopped %s\n", strings.ToUpper(phase), ack.Node, strings.Join(ack.Stopped, ", "))
		} else {
			fmt.Printf("[%s] %s: OK\n", strings.ToUpper(phase), ack.Node)
		}
	}

	fmt.Printf("Local node %q drained; systemd may now stop storage services\n", node)
}

// guestEnumerationTimeout bounds how long a skipped drain waits on the
// spinifex.node.vms fan-out before giving up. It is intentionally short and
// independent of the drain --timeout: this only runs when the drain itself
// is being skipped, so it must never itself stall the stop. A var (like
// systemIsSystemRunning) so tests do not have to wait out a real timeout.
var guestEnumerationTimeout = 3 * time.Second

// vmStatusRunning mirrors vm.StateRunning's wire value. Not imported directly
// to avoid pulling the vm package into the CLI: VMInfo.Status crosses NATS as
// a plain string.
const vmStatusRunning = "running"

// reportGuestsLeftRunning names, at the given severity, every guest still
// running on this node, reusing the spinifex.node.vms fan-out `spx get vms`
// already relies on. Enumeration failures always warn, never escalate.
func reportGuestsLeftRunning(severe bool, msg string) {
	cfg, nc, err := loadConfigAndConnectFn()
	if err != nil {
		slog.Warn("could not connect to check for guests left running", "error", err)
		return
	}
	defer nc.Close()
	node := cfg.Node

	responses, err := collectResponses(nc, "spinifex.node.vms", guestEnumerationTimeout)
	if err != nil {
		slog.Warn("could not enumerate local guests", "node", node, "error", err)
		return
	}

	var running []string
	for _, data := range responses {
		var resp types.NodeVMsResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}
		if resp.Node != node {
			continue
		}
		for _, v := range resp.VMs {
			if v.Status == vmStatusRunning {
				running = append(running, v.InstanceID)
			}
		}
	}

	if len(running) == 0 {
		return
	}
	sort.Strings(running)
	if severe {
		slog.Error(msg, "node", node, "instances", running)
	} else {
		slog.Warn(msg, "node", node, "instances", running)
	}
}

// warnIfGuestsLeftRunning reports, at WARN level, every guest still running
// on this node when the drain gate has just decided to skip a stop because a
// restart is already queued.
func warnIfGuestsLeftRunning() {
	reportGuestsLeftRunning(false, "skipped drain leaves guests running unsupervised; they will lose their data path if the spinifex stack does not come back")
}

// localShutdownACKRetryBaseDelay/MaxDelay set the backoff between GATE/DRAIN
// ACK attempts: NATS bounces a request instantly when nothing is subscribed
// yet, so a single miss right after a daemon restart is not a real failure.
var (
	localShutdownACKRetryBaseDelay = 250 * time.Millisecond
	localShutdownACKRetryMaxDelay  = 2 * time.Second
)

// localDrainRequest builds a phase request scoped to one node. Target is what
// keeps it local: the phase subjects are fan-out, so an untargeted request
// gates and drains every node in the cluster off one node's target stop.
func localDrainRequest(phase, node string, timeout time.Duration) daemon.ShutdownRequest {
	return daemon.ShutdownRequest{
		Phase:   phase,
		Timeout: int(timeout.Seconds()),
		Target:  node,
	}
}

// summariseShutdownACKs splits a phase's ACKs into the count that completed
// cleanly and the names of the nodes that answered with an error.
func summariseShutdownACKs(acks []daemon.ShutdownACK) (completed int, failed []string) {
	for _, ack := range acks {
		if ack.Error != "" {
			failed = append(failed, ack.Node)
			continue
		}
		completed++
	}
	return completed, failed
}

// collectShutdownACKsFn indirects collectShutdownACKs so tests can simulate a
// slow-to-resubscribe daemon without a real NATS round trip.
var collectShutdownACKsFn = collectShutdownACKs

// collectLocalShutdownACK publishes a shutdown-phase request and returns the
// ACK from the local node, retrying with backoff bounded by timeout as a
// whole. Returns an error once the budget is exhausted with no ACK.
func collectLocalShutdownACK(nc *nats.Conn, topic string, reqData []byte, node string, timeout time.Duration) (daemon.ShutdownACK, error) {
	deadline := time.Now().Add(timeout)
	delay := localShutdownACKRetryBaseDelay
	attempts := 0
	loggedRetry := false

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		attempts++

		acks, err := collectShutdownACKsFn(nc, topic, reqData, 1, node, remaining)
		if err != nil {
			return daemon.ShutdownACK{}, fmt.Errorf("collecting ACK from local node %q: %w", node, err)
		}
		if len(acks) > 0 {
			return acks[0], nil
		}

		remaining = time.Until(deadline)
		if remaining <= 0 {
			break
		}
		// Log once, not per attempt: this runs in ExecStop, so its output
		// lands in the journal on every single stop, and most stops recover
		// within the first attempt or two.
		if !loggedRetry {
			slog.Warn("no ACK yet for local node, retrying (daemon may still be re-subscribing)",
				"node", node, "topic", topic)
			loggedRetry = true
		}

		sleep := min(delay, remaining)
		time.Sleep(sleep)
		delay *= 2
		if delay > localShutdownACKRetryMaxDelay {
			delay = localShutdownACKRetryMaxDelay
		}
	}

	if attempts == 0 {
		return daemon.ShutdownACK{}, fmt.Errorf("no time budget to wait for local node %q ACK (timeout=%s)", node, timeout)
	}
	return daemon.ShutdownACK{}, fmt.Errorf("no ACK from local node %q after %d attempt(s) within %s", node, attempts, timeout)
}

// runClusterDrainDHCP asks every vpcd to DHCPRELEASE all external-pool leases
// it currently holds, returning them to the upstream DHCP server. Intended for
// the teardown path: an env reset otherwise strands held leases until TTL
// because vpcd's Stop() preserves them for adopt-on-reboot. Best-effort — if
// the cluster is already partly down it warns and exits 0 so teardown proceeds.
func runClusterDrainDHCP(cmd *cobra.Command, args []string) {
	timeout, _ := cmd.Flags().GetDuration("timeout")
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	_, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: drain-dhcp could not connect (cluster may be down): %v\n", err)
		return
	}
	defer nc.Close()

	total, responders := drainDHCPLeases(nc, timeout)
	fmt.Printf("DHCP drain complete: released %d lease(s) from %d vpcd responder(s)\n", total, responders)
}

// drainDHCPLeases publishes a drain request to vpc.dhcp.drain and collects
// replies until timeout — one responder per AZ (vpcd uses a per-AZ queue
// group). Returns the summed released count and the number of responders.
func drainDHCPLeases(nc *nats.Conn, timeout time.Duration) (released, responders int) {
	reqData, err := json.Marshal(struct{}{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 0, 0
	}

	inbox := nats.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: drain-dhcp subscribe failed: %v\n", err)
		return 0, 0
	}
	defer sub.Unsubscribe()

	if err := nc.PublishRequest(dhcp.TopicDrain, inbox, reqData); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: drain-dhcp publish failed: %v\n", err)
		return 0, 0
	}
	nc.Flush()

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := sub.NextMsg(remaining)
		if err != nil {
			break
		}
		var reply struct {
			Released int    `json:"released"`
			Error    string `json:"error,omitempty"`
		}
		if err := json.Unmarshal(msg.Data, &reply); err != nil {
			continue
		}
		responders++
		if reply.Error != "" {
			fmt.Fprintf(os.Stderr, "  vpcd drain error: %s\n", reply.Error)
			continue
		}
		released += reply.Released
	}
	return released, responders
}

// collectShutdownACKs publishes a shutdown request and collects ACKs from nodes.
// When nodeFilter is non-empty, only ACKs from that node count toward nodeCount
// and all others are ignored (used by the single-node local-drain path).
func collectShutdownACKs(nc *nats.Conn, topic string, reqData []byte, nodeCount int, nodeFilter string, timeout time.Duration) ([]daemon.ShutdownACK, error) {
	inbox := nats.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe to inbox: %w", err)
	}
	defer sub.Unsubscribe()

	if err := nc.PublishRequest(topic, inbox, reqData); err != nil {
		return nil, fmt.Errorf("failed to publish request: %w", err)
	}
	nc.Flush()

	var acks []daemon.ShutdownACK
	deadline := time.Now().Add(timeout)
	for len(acks) < nodeCount {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := sub.NextMsg(remaining)
		if err != nil {
			break
		}
		var ack daemon.ShutdownACK
		if err := json.Unmarshal(msg.Data, &ack); err != nil {
			continue
		}
		if nodeFilter != "" && ack.Node != nodeFilter {
			continue
		}
		acks = append(acks, ack)
	}
	return acks, nil
}
