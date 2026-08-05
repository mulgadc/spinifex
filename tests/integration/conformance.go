//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/mulgadc/spinifex/internal/awsmodel"
)

type conformanceFinding struct {
	service   awsmodel.Service
	operation string
	violation awsmodel.Violation
	count     int
}

type conformanceDiagnostic struct {
	service   awsmodel.Service
	operation string
	message   string
	count     int
}

// conformanceCollector is shared across the integration suite. Findings for
// services in the checked-in promotion policy become blocking in fail mode.
type conformanceCollector struct {
	mu                       sync.Mutex
	checked                  int
	checkedByService         map[awsmodel.Service]int
	errorChecked             int
	errorCheckedByService    map[awsmodel.Service]int
	errorUnmodelled          int
	errorUnmodelledByService map[awsmodel.Service]int
	findings                 map[string]*conformanceFinding
	diagnostics              map[string]*conformanceDiagnostic
}

func newConformanceCollector() *conformanceCollector {
	return &conformanceCollector{
		checkedByService:         make(map[awsmodel.Service]int),
		errorCheckedByService:    make(map[awsmodel.Service]int),
		errorUnmodelledByService: make(map[awsmodel.Service]int),
		findings:                 make(map[string]*conformanceFinding),
		diagnostics:              make(map[string]*conformanceDiagnostic),
	}
}

var suiteConformance = newConformanceCollector()

func (c *conformanceCollector) record(service awsmodel.Service, operation string, body []byte) {
	violations, err := awsmodel.ValidateResponse(service, operation, body)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.checked++
	c.checkedByService[service]++
	if err != nil {
		key := string(service) + "\x00" + operation + "\x00" + err.Error()
		diagnostic := c.diagnostics[key]
		if diagnostic == nil {
			diagnostic = &conformanceDiagnostic{service: service, operation: operation, message: err.Error()}
			c.diagnostics[key] = diagnostic
		}
		diagnostic.count++
		return
	}
	c.recordViolationsLocked(service, operation, violations)
}

func (c *conformanceCollector) recordError(service awsmodel.Service, operation string, status int, body []byte) {
	var (
		violations []awsmodel.Violation
		modelled   bool
		err        error
	)
	if service == awsmodel.EC2 {
		violations, err = awsmodel.ValidateEC2ErrorResponse(status, body)
		modelled = true
	} else {
		violations, modelled, err = awsmodel.ValidateErrorResponse(service, operation, body)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		key := string(service) + "\x00" + operation + "\x00error\x00" + err.Error()
		diagnostic := c.diagnostics[key]
		if diagnostic == nil {
			diagnostic = &conformanceDiagnostic{service: service, operation: operation, message: err.Error()}
			c.diagnostics[key] = diagnostic
		}
		diagnostic.count++
		return
	}
	if !modelled {
		c.errorUnmodelled++
		c.errorUnmodelledByService[service]++
		return
	}
	c.errorChecked++
	c.errorCheckedByService[service]++
	c.recordViolationsLocked(service, operation, violations)
}

func (c *conformanceCollector) recordViolationsLocked(service awsmodel.Service, operation string, violations []awsmodel.Violation) {
	for _, violation := range violations {
		key := strings.Join([]string{string(service), operation, string(violation.Rule), violation.Path, violation.Message}, "\x00")
		finding := c.findings[key]
		if finding == nil {
			finding = &conformanceFinding{service: service, operation: operation, violation: violation}
			c.findings[key] = finding
		}
		finding.count++
	}
}

func (c *conformanceCollector) counts() (checked, violations, diagnostics int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, finding := range c.findings {
		violations += finding.count
	}
	for _, diagnostic := range c.diagnostics {
		diagnostics += diagnostic.count
	}
	return c.checked, violations, diagnostics
}

func (c *conformanceCollector) blocking(policy conformancePolicy, mode conformanceMode) int {
	if mode != conformanceModeFail {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blockingLocked(policy)
}

func (c *conformanceCollector) blockingLocked(policy conformancePolicy) int {
	blocking := 0
	for _, finding := range c.findings {
		if policy.isPromoted(finding.service) {
			blocking += finding.count
		}
	}
	for _, diagnostic := range c.diagnostics {
		if policy.isPromoted(diagnostic.service) {
			blocking += diagnostic.count
		}
	}
	return blocking
}

func (c *conformanceCollector) report(policy conformancePolicy, mode conformanceMode) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	violationCount := 0
	findings := make([]*conformanceFinding, 0, len(c.findings))
	for _, finding := range c.findings {
		violationCount += finding.count
		findings = append(findings, finding)
	}
	diagnosticCount := 0
	diagnostics := make([]*conformanceDiagnostic, 0, len(c.diagnostics))
	for _, diagnostic := range c.diagnostics {
		diagnosticCount += diagnostic.count
		diagnostics = append(diagnostics, diagnostic)
	}
	sort.Slice(findings, func(i, j int) bool {
		left, right := findings[i], findings[j]
		return fmt.Sprint(left.service, "\x00", left.operation, "\x00", left.violation.Path, "\x00", left.violation.Rule) <
			fmt.Sprint(right.service, "\x00", right.operation, "\x00", right.violation.Path, "\x00", right.violation.Rule)
	})
	sort.Slice(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		return fmt.Sprint(left.service, "\x00", left.operation, "\x00", left.message) <
			fmt.Sprint(right.service, "\x00", right.operation, "\x00", right.message)
	})

	var report strings.Builder
	blocking := 0
	if mode == conformanceModeFail {
		blocking = c.blockingLocked(policy)
	}
	promoted := make([]string, 0, len(policy.promoted))
	for _, service := range policy.services() {
		promoted = append(promoted, string(service))
	}
	fmt.Fprintf(&report, "AWS model conformance (%s): checked=%d errors_checked=%d errors_unmodelled=%d violations=%d decode_errors=%d blocking=%d promoted=%s\n",
		mode, c.checked, c.errorChecked, c.errorUnmodelled, violationCount, diagnosticCount, blocking, strings.Join(promoted, ","))

	checkedServices := make(map[awsmodel.Service]bool, len(c.checkedByService)+len(c.errorCheckedByService)+len(c.errorUnmodelledByService)+len(policy.promoted))
	for service := range c.checkedByService {
		checkedServices[service] = true
	}
	for service := range c.errorCheckedByService {
		checkedServices[service] = true
	}
	for service := range c.errorUnmodelledByService {
		checkedServices[service] = true
	}
	for service := range policy.promoted {
		checkedServices[service] = true
	}
	services := make([]awsmodel.Service, 0, len(checkedServices))
	for service := range checkedServices {
		services = append(services, service)
	}
	sort.Slice(services, func(i, j int) bool { return services[i] < services[j] })
	for _, service := range services {
		fmt.Fprintf(&report, "CHECKED %s success=%d errors=%d unmodelled_errors=%d promoted=%t\n",
			service, c.checkedByService[service], c.errorCheckedByService[service], c.errorUnmodelledByService[service], policy.isPromoted(service))
	}

	for _, finding := range findings {
		severity := "WARN"
		if mode == conformanceModeFail && policy.isPromoted(finding.service) {
			severity = "FAIL"
		}
		fmt.Fprintf(&report, "%s %s %s %s %s count=%d: %s\n",
			severity,
			finding.service, finding.operation, finding.violation.Path, finding.violation.Rule, finding.count, finding.violation.Message)
	}
	for _, diagnostic := range diagnostics {
		severity := "WARN"
		if mode == conformanceModeFail && policy.isPromoted(diagnostic.service) {
			severity = "FAIL"
		}
		fmt.Fprintf(&report, "%s %s %s decode_error count=%d: %s\n",
			severity,
			diagnostic.service, diagnostic.operation, diagnostic.count, diagnostic.message)
	}
	return strings.TrimSuffix(report.String(), "\n")
}

func conformanceMiddleware(next http.Handler, collector *conformanceCollector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service, operation, ok := resolveConformanceRequest(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		capture := &conformanceResponseWriter{ResponseWriter: w}
		next.ServeHTTP(capture, r)
		status := capture.status
		if status == 0 {
			status = http.StatusOK
		}
		if status >= 200 && status < 300 {
			collector.record(service, operation, capture.body.Bytes())
		} else if errorCodeConformanceService(service) {
			collector.recordError(service, operation, status, capture.body.Bytes())
		}
	})
}

func errorCodeConformanceService(service awsmodel.Service) bool {
	switch service {
	case awsmodel.EC2, awsmodel.IAM, awsmodel.STS, awsmodel.ECS, awsmodel.ElasticLoadBalancingV2:
		return true
	default:
		return false
	}
}

type conformanceResponseWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *conformanceResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *conformanceResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.body.Write(body)
	return w.ResponseWriter.Write(body)
}

func (w *conformanceResponseWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *conformanceResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func resolveConformanceRequest(r *http.Request) (awsmodel.Service, string, bool) {
	service, ok := conformanceService(r)
	if !ok {
		return "", "", false
	}

	var operation string
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		if separator := strings.LastIndex(target, "."); separator >= 0 {
			operation = target[separator+1:]
		} else {
			operation = target
		}
	} else if service != awsmodel.S3 {
		operation = queryAction(r)
	}
	if operation == "" {
		return "", "", false
	}

	model, err := awsmodel.Load(service)
	if err != nil {
		return "", "", false
	}
	if _, modelled := model.Operation(operation); !modelled {
		return "", "", false
	}
	return service, operation, true
}

func conformanceService(r *http.Request) (awsmodel.Service, bool) {
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		prefix := target
		if separator := strings.LastIndex(target, "."); separator >= 0 {
			prefix = target[:separator]
		}
		switch prefix {
		case "AmazonEC2ContainerServiceV20141113":
			return awsmodel.ECS, true
		case "AmazonEC2ContainerRegistry_V20150921":
			return awsmodel.ECR, true
		case "CertificateManager":
			return awsmodel.ACM, true
		}
	}

	authorization := r.Header.Get("Authorization")
	credentialAt := strings.Index(authorization, "Credential=")
	if credentialAt >= 0 {
		credential := authorization[credentialAt+len("Credential="):]
		if end := strings.IndexAny(credential, " ,"); end >= 0 {
			credential = credential[:end]
		}
		parts := strings.Split(credential, "/")
		if len(parts) >= 5 && parts[len(parts)-1] == "aws4_request" {
			return modelService(parts[len(parts)-2])
		}
	}

	// AssumeRoleWithWebIdentity is intentionally unsigned and is the only
	// anonymous modelled request in this harness.
	if queryAction(r) == "AssumeRoleWithWebIdentity" {
		return awsmodel.STS, true
	}
	return "", false
}

func modelService(signatureService string) (awsmodel.Service, bool) {
	switch signatureService {
	case "acm":
		return awsmodel.ACM, true
	case "ec2":
		return awsmodel.EC2, true
	case "ecr":
		return awsmodel.ECR, true
	case "ecs":
		return awsmodel.ECS, true
	case "elasticloadbalancing":
		return awsmodel.ElasticLoadBalancingV2, true
	case "iam":
		return awsmodel.IAM, true
	case "s3":
		return awsmodel.S3, true
	case "sts":
		return awsmodel.STS, true
	default:
		return "", false
	}
}

func queryAction(r *http.Request) string {
	if action := r.URL.Query().Get("Action"); action != "" {
		return action
	}
	if r.Body == nil {
		return ""
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return ""
	}
	return values.Get("Action")
}
