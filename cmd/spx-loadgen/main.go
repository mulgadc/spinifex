// spx-loadgen drives a measured request load at a Spinifex control plane and
// reports per-operation latency distributions as JSON and as a summary.
//
// It is the measuring half of the stress harness: scripts/stress in the mulga
// repo owns tenants, workloads and teardown, and spawns this to produce the
// numbers. Driving the AWS CLI instead costs over a second of interpreter
// start-up per call, which is larger than the latency being measured.
//
// Two pacing modes, reported separately because they answer different
// questions. Closed loop holds N callers in flight and slows down when the
// cluster does, which is what a fixed pool of console users looks like. Open
// loop issues a fixed rate regardless and times each request from when it was
// due, which is the only way to watch a queue form.
//
// Usage:
//
//	spx-loadgen -endpoint https://api.spx3.com -region us-west-1 \
//	    -tenants run/tenants.txt -closed 1,2,4,8 -open 10,20,40 \
//	    -duration 30s -out run/loadgen.json
//
// Tenants are read from the harness's tenants.txt: "<account> <name> <profile>"
// per line, profile naming a shared-config AWS profile. -profile runs against
// one profile without a file.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/spinifex/loadgen"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "spx-loadgen: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	endpoint  string
	region    string
	caBundle  string
	tenants   string
	profile   string
	ops       string
	closed    string
	open      string
	duration  time.Duration
	timeout   time.Duration
	readP99   time.Duration
	writeP99  time.Duration
	expected  string
	out       string
	failOnSLO bool
}

func run() error {
	var opt options
	flag.StringVar(&opt.endpoint, "endpoint", "", "control-plane endpoint, e.g. https://api.spx3.com")
	flag.StringVar(&opt.region, "region", "us-west-1", "region")
	flag.StringVar(&opt.caBundle, "ca-bundle", "", "CA bundle, only for an endpoint serving the cluster's own certificate")
	flag.StringVar(&opt.tenants, "tenants", "", "tenants file: '<account> <name> <profile>' per line")
	flag.StringVar(&opt.profile, "profile", "", "single AWS profile to run as, instead of -tenants")
	flag.StringVar(&opt.ops, "ops", strings.Join(loadgen.DefaultOps, ","),
		"operations to drive, comma separated (known: "+strings.Join(loadgen.OpNames(), ", ")+")")
	flag.StringVar(&opt.closed, "closed", "", "closed-loop concurrency stages, e.g. 1,2,4,8")
	flag.StringVar(&opt.open, "open", "", "open-loop rate stages in requests/second, e.g. 10,20,40")
	flag.DurationVar(&opt.duration, "duration", 30*time.Second, "duration of each stage")
	flag.DurationVar(&opt.timeout, "timeout", 30*time.Second, "per-request timeout")
	flag.DurationVar(&opt.readP99, "slo-read-p99", 500*time.Millisecond, "p99 SLO for read operations")
	flag.DurationVar(&opt.writeP99, "slo-write-p99", 2*time.Second, "p99 SLO for write operations")
	flag.StringVar(&opt.expected, "expected-errors", "",
		"error codes that are not failures, comma separated (quota rejections belong here)")
	flag.StringVar(&opt.out, "out", "", "write the JSON report here (default stdout only)")
	flag.BoolVar(&opt.failOnSLO, "fail-on-breach", false, "exit non-zero if any stage breaches the SLO")
	flag.Parse()

	if opt.endpoint == "" {
		return errors.New("-endpoint is required")
	}
	if opt.closed == "" && opt.open == "" {
		return errors.New("nothing to run: give -closed, -open, or both")
	}

	ops, err := loadgen.ResolveOps(strings.Split(opt.ops, ","))
	if err != nil {
		return err
	}
	stages, err := buildStages(opt)
	if err != nil {
		return err
	}
	profiles, accounts, err := readTenants(opt)
	if err != nil {
		return err
	}

	// The transport must hold at least as many connections as the widest stage
	// or the run measures TLS handshakes rather than the control plane.
	dial := loadgen.Dial{
		Endpoint: opt.endpoint, Region: opt.region, CABundle: opt.caBundle,
		MaxIdleConnsPerHost: maxConcurrency(stages), Timeout: opt.timeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	targets := make([]*loadgen.Target, 0, len(profiles))
	for index, profile := range profiles {
		clients, err := loadgen.NewClients(dial, profile)
		if err != nil {
			return err
		}
		target := &loadgen.Target{Account: accounts[index], Profile: profile, Clients: clients}
		if loadgen.NeedsVPC(ops) {
			if err := loadgen.ResolveVPC(ctx, target); err != nil {
				return err
			}
		}
		if loadgen.NeedsVolume(ops) {
			if err := loadgen.ResolveVolume(ctx, target); err != nil {
				return err
			}
		}
		targets = append(targets, target)
	}

	slo := loadgen.SLO{
		ReadP99: opt.readP99, WriteP99: opt.writeP99,
		ReadP99MS: float64(opt.readP99.Milliseconds()), WriteP99MS: float64(opt.writeP99.Milliseconds()),
		ExpectedErrorCodes: splitNonEmpty(opt.expected),
	}

	report, err := loadgen.Run(ctx, targets, ops, stages, slo)
	if err != nil {
		return err
	}
	report.Endpoint = opt.endpoint
	report.Region = opt.region

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	if opt.out != "" {
		if err := os.WriteFile(opt.out, append(encoded, '\n'), 0o644); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}
	fmt.Print(loadgen.Summary(report))

	if opt.failOnSLO && len(report.FirstBreach) > 0 {
		return errors.New("SLO breached")
	}
	return nil
}

// buildStages runs every closed-loop stage before any open-loop one. Mixing
// them would leave each mode measuring the queue the other left behind.
func buildStages(opt options) ([]loadgen.Stage, error) {
	var stages []loadgen.Stage
	for _, value := range splitNonEmpty(opt.closed) {
		concurrency, err := strconv.Atoi(value)
		if err != nil || concurrency < 1 {
			return nil, fmt.Errorf("-closed: %q is not a positive integer", value)
		}
		stages = append(stages, loadgen.Stage{
			Mode: loadgen.ModeClosed, Concurrency: concurrency, Duration: opt.duration,
		})
	}
	for _, value := range splitNonEmpty(opt.open) {
		rps, err := strconv.ParseFloat(value, 64)
		if err != nil || rps <= 0 {
			return nil, fmt.Errorf("-open: %q is not a positive rate", value)
		}
		stages = append(stages, loadgen.Stage{
			Mode: loadgen.ModeOpen, RPS: rps, Duration: opt.duration,
		})
	}
	return stages, nil
}

// readTenants returns the profiles to run as and the account each belongs to.
func readTenants(opt options) (profiles, accounts []string, err error) {
	if opt.profile != "" {
		return []string{opt.profile}, []string{opt.profile}, nil
	}
	if opt.tenants == "" {
		return nil, nil, errors.New("give -tenants or -profile")
	}

	file, err := os.Open(opt.tenants)
	if err != nil {
		return nil, nil, fmt.Errorf("read tenants: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		accounts = append(accounts, fields[0])
		profiles = append(profiles, fields[2])
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("read tenants: %w", err)
	}
	if len(profiles) == 0 {
		return nil, nil, fmt.Errorf("no tenants in %s", opt.tenants)
	}
	return profiles, accounts, nil
}

func maxConcurrency(stages []loadgen.Stage) int {
	widest := 16
	for _, stage := range stages {
		if stage.Concurrency > widest {
			widest = stage.Concurrency
		}
		if int(stage.RPS) > widest {
			widest = int(stage.RPS)
		}
	}
	return widest
}

func splitNonEmpty(value string) []string {
	var out []string
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
