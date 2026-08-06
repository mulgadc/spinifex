package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	handlers_bedrock "github.com/mulgadc/spinifex/spinifex/handlers/bedrock"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
)

var ochreEndpointCmd = &cobra.Command{
	Use:   "endpoint",
	Short: "Drive the serving-endpoint lifecycle for self-host models",
	Long: `Operator surface over the daemon's serving-endpoint lifecycle: request an
endpoint for a staged model, inspect its state, and tear it down.

The gateway requests endpoints on its own: an invoke for a model with no
endpoint returns ModelNotReadyException and launches one in the background,
and the daemon reclaims an endpoint that has been idle past its TTL. These
commands drive the same lifecycle by hand, for staging a model ahead of first
use or taking its GPU back early.`,
}

var ochreEndpointEnsureCmd = &cobra.Command{
	Use:   "ensure",
	Short: "Request a serving endpoint for a self-host model",
	Long: `ensure asks the daemon to bring up a serving VM for --model-id, which must
already have staged weights.

Idempotent: a model whose endpoint is already STARTING or READY returns the
current record rather than launching a second VM.

The daemon replies STARTING as soon as it has claimed the model, and the
launch continues in the background. Pass --wait to poll until the endpoint
reaches READY and report how long the cold start took.`,
	Run: runOchreEndpointEnsure,
}

var ochreEndpointDescribeCmd = &cobra.Command{
	Use:   "describe",
	Short: "Show a model's current serving-endpoint record",
	Run:   runOchreEndpointDescribe,
}

var ochreEndpointListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every serving-endpoint record",
	Run:   runOchreEndpointList,
}

var ochreEndpointDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Tear down a model's serving endpoint and release its VM",
	Long: `delete moves a READY endpoint to DRAINING and tears its VM down, releasing
the GPU. Idempotent: an endpoint that is already absent reports success.`,
	Run: runOchreEndpointDelete,
}

func init() {
	ochreCmd.AddCommand(ochreEndpointCmd)
	ochreEndpointCmd.AddCommand(ochreEndpointEnsureCmd)
	ochreEndpointCmd.AddCommand(ochreEndpointDescribeCmd)
	ochreEndpointCmd.AddCommand(ochreEndpointListCmd)
	ochreEndpointCmd.AddCommand(ochreEndpointDeleteCmd)

	ochreEndpointEnsureCmd.Flags().String("model-id", "", "Catalog model ID to bring an endpoint up for (required)")
	ochreEndpointEnsureCmd.Flags().Bool("wait", false, "Poll until the endpoint is READY and report the elapsed cold start")
	ochreEndpointEnsureCmd.Flags().Duration("timeout", defaultEndpointWaitTimeout, "How long --wait polls before giving up")
	_ = ochreEndpointEnsureCmd.MarkFlagRequired("model-id")

	ochreEndpointDescribeCmd.Flags().String("model-id", "", "Model ID to describe (required)")
	_ = ochreEndpointDescribeCmd.MarkFlagRequired("model-id")

	ochreEndpointDeleteCmd.Flags().String("model-id", "", "Model ID whose endpoint to tear down (required)")
	_ = ochreEndpointDeleteCmd.MarkFlagRequired("model-id")
}

const (
	// defaultEndpointWaitTimeout must stay comfortably above the daemon's own
	// readiness bound. The daemon starts its clock once the VM is launched,
	// which is up to a minute after this one starts, so an equal value expires
	// here first and reports a bare client timeout instead of the daemon's
	// abort, which says why the launch actually failed.
	defaultEndpointWaitTimeout = 20 * time.Minute
	// endpointPollInterval trades responsiveness against KV read volume. A
	// cold start is minutes, so seconds of granularity is plenty.
	endpointPollInterval = 2 * time.Second
)

// errEndpointLaunchAborted reports a STARTING endpoint that went back to
// ABSENT. A failed launch deletes the record rather than parking it in a
// terminal state, so its disappearance IS the failure signal.
var errEndpointLaunchAborted = errors.New("endpoint launch aborted: the record returned to ABSENT, which is how a failed launch or readiness timeout reports itself")

// errEndpointWaitTimeout reports that the poll window closed with the
// endpoint still STARTING. The endpoint is deliberately left running.
var errEndpointWaitTimeout = errors.New("timed out waiting for the endpoint to become READY")

// endpointWaitClock indirects time so the poll loop is testable without
// sleeping through a real cold start.
type endpointWaitClock struct {
	now   func() time.Time
	sleep func(time.Duration)
}

func realEndpointWaitClock() endpointWaitClock {
	return endpointWaitClock{now: time.Now, sleep: time.Sleep}
}

// waitForEndpointReady polls Describe until the endpoint is READY, has gone
// back to ABSENT, or the timeout expires, and returns the record it settled
// on plus how long that took.
func waitForEndpointReady(ctx context.Context, svc handlers_bedrock.EndpointService, modelID string,
	timeout time.Duration, clock endpointWaitClock) (handlers_bedrock.EndpointRecord, time.Duration, error) {
	start := clock.now()
	for {
		out, err := svc.Describe(ctx, &handlers_bedrock.DescribeEndpointInput{ModelID: modelID}, utils.GlobalAccountID)
		if err != nil {
			return handlers_bedrock.EndpointRecord{}, clock.now().Sub(start), err
		}
		switch out.Endpoint.State {
		case handlers_bedrock.StateReady:
			return out.Endpoint, clock.now().Sub(start), nil
		case handlers_bedrock.StateAbsent:
			return out.Endpoint, clock.now().Sub(start), errEndpointLaunchAborted
		}

		// Check the deadline only after a Describe, so a timeout already at
		// zero still reports the endpoint's actual state rather than nothing.
		if elapsed := clock.now().Sub(start); elapsed >= timeout {
			return out.Endpoint, elapsed, errEndpointWaitTimeout
		}
		clock.sleep(endpointPollInterval)
	}
}

// formatEndpointRecord renders one record as aligned key/value lines, omitting
// fields that are only set once a launch has progressed far enough to have them.
func formatEndpointRecord(rec handlers_bedrock.EndpointRecord) string {
	rows := [][2]string{
		{"Model ID", rec.ModelID},
		{"State", string(rec.State)},
	}
	if rec.InstanceID != "" {
		rows = append(rows, [2]string{"Instance ID", rec.InstanceID})
	}
	if rec.NodeID != "" {
		rows = append(rows, [2]string{"Node ID", rec.NodeID})
	}
	if rec.BaseURL != "" {
		rows = append(rows, [2]string{"Base URL", rec.BaseURL})
	}
	if rec.WeightsVolumeID != "" {
		rows = append(rows, [2]string{"Weights volume", rec.WeightsVolumeID})
	}
	if !rec.CreatedAt.IsZero() {
		rows = append(rows, [2]string{"Created at", rec.CreatedAt.Format(time.RFC3339)})
	}
	if !rec.ReadyAt.IsZero() {
		rows = append(rows, [2]string{"Ready at", rec.ReadyAt.Format(time.RFC3339)})
		if !rec.CreatedAt.IsZero() {
			rows = append(rows, [2]string{"Startup", rec.ReadyAt.Sub(rec.CreatedAt).Round(time.Second).String()})
		}
	}
	rows = append(rows, reclaimRows(rec)...)

	out := ""
	for _, row := range rows {
		out += fmt.Sprintf("%-15s %s\n", row[0]+":", row[1])
	}
	return out
}

// reclaimRows renders what the daemon's idle sweep last observed, so an
// operator asking why an endpoint was or was not reclaimed can see the inputs
// rather than infer them. Only meaningful for a READY endpoint: no other state
// is swept.
//
// "Idle for" is measured from the record's LastActive, which falls back to
// ReadyAt, so an endpoint that has been quiet since launch reads as idle since
// launch rather than since the zero time.
func reclaimRows(rec handlers_bedrock.EndpointRecord) [][2]string {
	if rec.State != handlers_bedrock.StateReady {
		return nil
	}
	rows := [][2]string{{"In flight", strconv.Itoa(rec.InFlight)}}
	if since := rec.LastActive(); !since.IsZero() {
		rows = append(rows,
			[2]string{"Last active", since.Format(time.RFC3339)},
			[2]string{"Idle for", time.Since(since).Round(time.Second).String()})
	}
	if rec.Pinned {
		rows = append(rows, [2]string{"Pinned", "yes (never reclaimed or evicted)"})
	}
	if rec.ScrapeFailures > 0 {
		rows = append(rows, [2]string{"Scrape failures", strconv.Itoa(rec.ScrapeFailures)})
	}
	return rows
}

// listEndpointsOutput renders 'ochre endpoint list'. Split from its Run
// function so it is testable against a fake service with no NATS connection.
func listEndpointsOutput(ctx context.Context, svc handlers_bedrock.EndpointService) (string, error) {
	out, err := svc.List(ctx, &handlers_bedrock.ListEndpointsInput{}, utils.GlobalAccountID)
	if err != nil {
		return "", err
	}
	if len(out.Endpoints) == 0 {
		return "No serving endpoints.", nil
	}

	tableData := pterm.TableData{{"MODEL ID", "STATE", "INSTANCE ID", "BASE URL"}}
	for _, e := range out.Endpoints {
		tableData = append(tableData, []string{e.ModelID, string(e.State), e.InstanceID, e.BaseURL})
	}
	return pterm.DefaultTable.WithHasHeader().WithData(tableData).Srender()
}

// runEnsureEndpoint is the testable core of 'ochre endpoint ensure': request
// the endpoint, then optionally wait for it. Returns the message to print.
func runEnsureEndpoint(ctx context.Context, svc handlers_bedrock.EndpointService, modelID string,
	wait bool, timeout time.Duration, clock endpointWaitClock) (string, error) {
	out, err := svc.Ensure(ctx, &handlers_bedrock.EnsureEndpointInput{ModelID: modelID}, utils.GlobalAccountID)
	if err != nil {
		return "", err
	}
	if !wait {
		return fmt.Sprintf("Endpoint for %s is %s.\n\n%s", modelID, out.Endpoint.State, formatEndpointRecord(out.Endpoint)), nil
	}

	// Already READY before any polling means this was a warm request, not a
	// cold start, so reporting an elapsed time would be misleading.
	if out.Endpoint.State == handlers_bedrock.StateReady {
		return fmt.Sprintf("Endpoint for %s was already READY.\n\n%s", modelID, formatEndpointRecord(out.Endpoint)), nil
	}

	fmt.Printf("Endpoint for %s is %s; waiting up to %s for READY ...\n", modelID, out.Endpoint.State, timeout)
	rec, elapsed, err := waitForEndpointReady(ctx, svc, modelID, timeout, clock)
	if err != nil {
		return "", fmt.Errorf("after %s: %w\n\n%s", elapsed.Round(time.Second), err, formatEndpointRecord(rec))
	}
	return fmt.Sprintf("✅ Endpoint for %s is READY after %s.\n\n%s", modelID, elapsed.Round(time.Second), formatEndpointRecord(rec)), nil
}

// endpointServiceFn indirects the NATS-backed client so the Run functions'
// connect/exit control flow can be tested without a live daemon.
var endpointServiceFn = func() (handlers_bedrock.EndpointService, func(), error) {
	_, nc, err := loadConfigAndConnectFn()
	if err != nil {
		return nil, nil, err
	}
	return handlers_bedrock.NewNATSEndpointService(nc), nc.Close, nil
}

func runOchreEndpointEnsure(cmd *cobra.Command, _ []string) {
	modelID, _ := cmd.Flags().GetString("model-id")
	wait, _ := cmd.Flags().GetBool("wait")
	timeout, _ := cmd.Flags().GetDuration("timeout")

	svc, closeFn, err := endpointServiceFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer closeFn()

	msg, err := runEnsureEndpoint(context.Background(), svc, modelID, wait, timeout, realEndpointWaitClock())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Println(msg)
}

func runOchreEndpointDescribe(cmd *cobra.Command, _ []string) {
	modelID, _ := cmd.Flags().GetString("model-id")

	svc, closeFn, err := endpointServiceFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer closeFn()

	out, err := svc.Describe(context.Background(), &handlers_bedrock.DescribeEndpointInput{ModelID: modelID}, utils.GlobalAccountID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Print(formatEndpointRecord(out.Endpoint))
}

func runOchreEndpointList(_ *cobra.Command, _ []string) {
	svc, closeFn, err := endpointServiceFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer closeFn()

	msg, err := listEndpointsOutput(context.Background(), svc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Println(msg)
}

func runOchreEndpointDelete(cmd *cobra.Command, _ []string) {
	modelID, _ := cmd.Flags().GetString("model-id")

	svc, closeFn, err := endpointServiceFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer closeFn()

	if _, err := svc.Delete(context.Background(), &handlers_bedrock.DeleteEndpointInput{ModelID: modelID}, utils.GlobalAccountID); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Printf("Endpoint for %s torn down.\n", modelID)
}
