package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

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

		acks, err := collectShutdownACKs(nc, topic, reqData, nodeCount, timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] Error: %v\n", strings.ToUpper(phase), err)
			if !force {
				fmt.Fprintf(os.Stderr, "Aborting shutdown. Use --force to continue despite errors.\n")
				os.Exit(1)
			}
		}

		// Unsubscribe from progress
		if progressSub != nil {
			progressSub.Unsubscribe()
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

		ackedCount := len(acks)
		if ackedCount < nodeCount && !force {
			fmt.Fprintf(os.Stderr, "[%s] Only %d/%d nodes responded. Use --force to continue.\n",
				strings.ToUpper(phase), ackedCount, nodeCount)
			os.Exit(1)
		}

		fmt.Printf("[%s] Complete (%d/%d nodes, %s)\n\n", strings.ToUpper(phase), ackedCount, nodeCount, time.Since(phaseStart).Round(time.Millisecond))
	}

	fmt.Printf("Cluster shutdown complete (%s)\n", time.Since(start).Round(time.Millisecond))
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
func collectShutdownACKs(nc *nats.Conn, topic string, reqData []byte, nodeCount int, timeout time.Duration) ([]daemon.ShutdownACK, error) {
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
		acks = append(acks, ack)
	}
	return acks, nil
}
