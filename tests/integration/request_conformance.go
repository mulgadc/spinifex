//go:build integration

package integration

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/mulgadc/spinifex/internal/awsmodel"
)

// requestFinding is one generated request whose outcome disagrees with the
// model: an acceptance request rejected as invalid, a rejection request that
// succeeded, or one refused with an error the operation does not declare.
type requestFinding struct {
	service awsmodel.Service
	request awsmodel.RequestCase
	result  awsmodel.RequestResult
	verdict awsmodel.Verdict
}

type requestServiceCounts struct {
	operations     int
	requests       int
	verdicts       map[awsmodel.Verdict]int
	inconclusive   map[string]int
	skippedMembers int
	unbroken       int
	// untested maps each operation that produced no request to the reason.
	untested map[string]string
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
		counts = &requestServiceCounts{verdicts: map[awsmodel.Verdict]int{}, inconclusive: map[string]int{}, untested: map[string]string{}}
		c.services[service] = counts
	}
	return counts
}

func (c *requestCollector) recordOperation(service awsmodel.Service, operation string, plan awsmodel.RequestPlan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	counts := c.serviceLocked(service)
	counts.operations++
	counts.skippedMembers += len(plan.Skipped)
	counts.unbroken += len(plan.Unbroken)
	if len(plan.Cases) == 0 {
		reasons := make([]string, len(plan.Skipped))
		for i, skip := range plan.Skipped {
			reasons[i] = skip.Path + ": " + skip.Reason
		}
		counts.untested[operation] = strings.Join(reasons, "; ")
	}
}

func (c *requestCollector) record(service awsmodel.Service, request awsmodel.RequestCase, result awsmodel.RequestResult, verdict awsmodel.Verdict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	counts := c.serviceLocked(service)
	counts.requests++
	counts.verdicts[verdict]++
	switch verdict {
	case awsmodel.VerdictFinding, awsmodel.VerdictUndeclaredError:
		c.findings = append(c.findings, requestFinding{service: service, request: request, result: result, verdict: verdict})
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
	untested := 0
	for _, counts := range c.services {
		total.operations += counts.operations
		total.requests += counts.requests
		total.skippedMembers += counts.skippedMembers
		total.unbroken += counts.unbroken
		untested += len(counts.untested)
		for verdict, count := range counts.verdicts {
			total.verdicts[verdict] += count
		}
	}
	fmt.Fprintf(&report, "AWS request conformance (%s): operations=%d untested_operations=%d requests=%d %s\n",
		mode, total.operations, untested, total.requests, verdictSummary(&total))

	for _, service := range slices.Sorted(maps.Keys(c.services)) {
		counts := c.services[service]
		fmt.Fprintf(&report, "REQUESTS %s operations=%d untested_operations=%d requests=%d %s promoted=%t\n",
			service, counts.operations, len(counts.untested), counts.requests, verdictSummary(counts), policy.isRequestPromoted(service))
		for _, operation := range slices.Sorted(maps.Keys(counts.untested)) {
			fmt.Fprintf(&report, "UNTESTED %s %s: %s\n", service, operation, counts.untested[operation])
		}
		codes := slices.SortedFunc(maps.Keys(counts.inconclusive), func(a, b string) int {
			return cmp.Or(cmp.Compare(counts.inconclusive[b], counts.inconclusive[a]), strings.Compare(a, b))
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
	slices.SortFunc(findings, func(a, b requestFinding) int {
		return cmp.Or(cmp.Compare(a.service, b.service), strings.Compare(a.request.Operation, b.request.Operation),
			strings.Compare(a.request.Path, b.request.Path), strings.Compare(a.request.Detail, b.request.Detail))
	})
	for _, finding := range findings {
		severity := "WARN"
		if mode == conformanceModeFail && policy.isRequestPromoted(finding.service) {
			severity = "FAIL"
		}
		request := finding.request
		switch {
		case finding.verdict == awsmodel.VerdictUndeclaredError:
			fmt.Fprintf(&report, "%s %s %s %s %s (%s) refused with undeclared %s: %s\n",
				severity, finding.service, request.Operation, request.Path, request.Constraint, request.Detail, finding.result.Code, finding.result.Message)
		case request.Acceptance():
			with := "with only required members"
			if request.Member != "" {
				with = "with " + request.Path + " set"
			}
			fmt.Fprintf(&report, "%s %s %s model-valid request %s rejected: %s: %s\n",
				severity, finding.service, request.Operation, with, finding.result.Code, finding.result.Message)
		default:
			fmt.Fprintf(&report, "%s %s %s %s %s (%s) accepted\n",
				severity, finding.service, request.Operation, request.Path, request.Constraint, request.Detail)
		}
	}
	return strings.TrimSuffix(report.String(), "\n")
}

func verdictSummary(counts *requestServiceCounts) string {
	return fmt.Sprintf("pass=%d findings=%d undeclared_errors=%d inconclusive=%d skipped_members=%d unbroken_constraints=%d",
		counts.verdicts[awsmodel.VerdictPass], counts.verdicts[awsmodel.VerdictFinding], counts.verdicts[awsmodel.VerdictUndeclaredError],
		counts.verdicts[awsmodel.VerdictInconclusive], counts.skippedMembers, counts.unbroken)
}
