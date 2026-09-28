//go:build integration

package integration

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/mulgadc/spinifex/internal/awsmodel"
)

// requestFinding is one generated request whose outcome disagrees with the
// model: an acceptance request rejected as invalid, or a rejection request
// that succeeded.
type requestFinding struct {
	service   awsmodel.Service
	operation string
	request   awsmodel.RequestCase
	result    awsmodel.RequestResult
}

type requestServiceCounts struct {
	operations   int
	requests     int
	verdicts     map[awsmodel.Verdict]int
	inconclusive map[string]int
	skipped      int
}

// requestCollector gathers the verdicts of the generated request sweep.
type requestCollector struct {
	mu       sync.Mutex
	services map[awsmodel.Service]*requestServiceCounts
	findings []requestFinding
}

func newRequestCollector() *requestCollector {
	return &requestCollector{services: map[awsmodel.Service]*requestServiceCounts{}}
}

var suiteRequestConformance = newRequestCollector()

func (c *requestCollector) serviceLocked(service awsmodel.Service) *requestServiceCounts {
	counts := c.services[service]
	if counts == nil {
		counts = &requestServiceCounts{verdicts: map[awsmodel.Verdict]int{}, inconclusive: map[string]int{}}
		c.services[service] = counts
	}
	return counts
}

func (c *requestCollector) recordOperation(service awsmodel.Service, skipped int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	counts := c.serviceLocked(service)
	counts.operations++
	counts.skipped += skipped
}

func (c *requestCollector) record(service awsmodel.Service, request awsmodel.RequestCase, result awsmodel.RequestResult, verdict awsmodel.Verdict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	counts := c.serviceLocked(service)
	counts.requests++
	counts.verdicts[verdict]++
	switch verdict {
	case awsmodel.VerdictFinding:
		c.findings = append(c.findings, requestFinding{service: service, operation: request.Operation, request: request, result: result})
	case awsmodel.VerdictInconclusive:
		code := result.Code
		if code == "" {
			code = fmt.Sprintf("status %d", result.Status)
		}
		counts.inconclusive[code]++
	}
}

func (c *requestCollector) blocking(policy conformancePolicy, mode conformanceMode) int {
	if mode != conformanceModeFail {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	blocking := 0
	for _, finding := range c.findings {
		if policy.isRequestPromoted(finding.service) {
			blocking++
		}
	}
	return blocking
}

func (c *requestCollector) report(policy conformancePolicy, mode conformanceMode) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var report strings.Builder
	total := requestServiceCounts{verdicts: map[awsmodel.Verdict]int{}}
	for _, counts := range c.services {
		total.operations += counts.operations
		total.requests += counts.requests
		total.skipped += counts.skipped
		for verdict, count := range counts.verdicts {
			total.verdicts[verdict] += count
		}
	}
	fmt.Fprintf(&report, "AWS request conformance (%s): operations=%d requests=%d pass=%d findings=%d inconclusive=%d skipped_members=%d\n",
		mode, total.operations, total.requests, total.verdicts[awsmodel.VerdictPass], total.verdicts[awsmodel.VerdictFinding],
		total.verdicts[awsmodel.VerdictInconclusive], total.skipped)

	for _, service := range slices.Sorted(maps.Keys(c.services)) {
		counts := c.services[service]
		fmt.Fprintf(&report, "REQUESTS %s operations=%d requests=%d pass=%d findings=%d inconclusive=%d skipped_members=%d promoted=%t\n",
			service, counts.operations, counts.requests, counts.verdicts[awsmodel.VerdictPass], counts.verdicts[awsmodel.VerdictFinding],
			counts.verdicts[awsmodel.VerdictInconclusive], counts.skipped, policy.isRequestPromoted(service))
		codes := slices.SortedFunc(maps.Keys(counts.inconclusive), func(a, b string) int {
			if counts.inconclusive[a] != counts.inconclusive[b] {
				return counts.inconclusive[b] - counts.inconclusive[a]
			}
			return strings.Compare(a, b)
		})
		if len(codes) > 0 {
			parts := make([]string, len(codes))
			for i, code := range codes {
				parts[i] = fmt.Sprintf("%s=%d", code, counts.inconclusive[code])
			}
			fmt.Fprintf(&report, "INCONCLUSIVE %s %s\n", service, strings.Join(parts, " "))
		}
	}

	findings := slices.Clone(c.findings)
	sort.Slice(findings, func(i, j int) bool {
		left, right := findings[i], findings[j]
		return fmt.Sprint(left.service, "\x00", left.operation, "\x00", left.request.Path, "\x00", left.request.Detail) <
			fmt.Sprint(right.service, "\x00", right.operation, "\x00", right.request.Path, "\x00", right.request.Detail)
	})
	for _, finding := range findings {
		severity := "WARN"
		if mode == conformanceModeFail && policy.isRequestPromoted(finding.service) {
			severity = "FAIL"
		}
		if finding.request.Acceptance() {
			with := "with only required members"
			if finding.request.Member != "" {
				with = "with " + finding.request.Path + " set"
			}
			fmt.Fprintf(&report, "%s %s %s model-valid request %s rejected: %s: %s\n",
				severity, finding.service, finding.operation, with, finding.result.Code, finding.result.Message)
			continue
		}
		fmt.Fprintf(&report, "%s %s %s %s %s (%s) accepted\n",
			severity, finding.service, finding.operation, finding.request.Path, finding.request.Constraint, finding.request.Detail)
	}
	return strings.TrimSuffix(report.String(), "\n")
}
