package awsmodel_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	. "github.com/mulgadc/spinifex/internal/awsmodel"
)

func testPageSet(t *testing.T) PageSet {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("..", "..", "docs", "coverage", "pages.json"))
	if err != nil {
		t.Fatal(err)
	}
	pages, err := ParsePageSet(contents)
	if err != nil {
		t.Fatal(err)
	}
	return pages
}

func TestParsePageSetAcceptsTheCheckedInFile(t *testing.T) {
	t.Parallel()
	pages := testPageSet(t)

	if len(pages.Services) == 0 {
		t.Fatal("no service pages are configured")
	}
	// Predastore serves S3, so the page says so rather than crediting Spinifex.
	if implementer := pages.Services[S3].ImplementedBy; implementer != "Predastore" {
		t.Errorf("S3 is implemented by %q, want Predastore", implementer)
	}
}

func TestParsePageSetRejectsUnpublishableMetadata(t *testing.T) {
	t.Parallel()
	// A well-formed index, so each case below fails only for its own reason.
	index := `"index":{"name":"All","title":"All","seoTitle":"` + strings.Repeat("x", 39) +
		` — Spinifex Docs","description":"` + strings.Repeat("x", 155) + `","tags":["aws"]}`
	head := `{"category":"Coverage","basePath":"/coverage",`

	for name, contents := range map[string]string{
		"unknown service":  head + index + `,"services":{"notaservice":{}}}`,
		"missing category": `{"basePath":"/coverage",` + index + `,"services":{}}`,
		"missing basePath": `{"category":"Coverage",` + index + `,"services":{}}`,
		"unknown field":    head + `"colour":"blue",` + index + `,"services":{}}`,
		"index with a slug": head + `"index":{"slug":"aws","name":"All","title":"All","seoTitle":"` +
			strings.Repeat("x", 39) + ` — Spinifex Docs","description":"` + strings.Repeat("x", 155) + `","tags":["aws"]}}`,
		"short seoTitle": head + `"index":{"name":"All","title":"All",` +
			`"seoTitle":"Too short — Spinifex Docs","description":"` + strings.Repeat("x", 155) + `","tags":["aws"]}}`,
		"missing suffix": head + `"index":{"name":"All","title":"All",` +
			`"seoTitle":"` + strings.Repeat("x", 55) + `","description":"` + strings.Repeat("x", 155) + `","tags":["aws"]}}`,
		"short description": head + `"index":{"name":"All","title":"All",` +
			`"seoTitle":"` + strings.Repeat("x", 39) + ` — Spinifex Docs","description":"too short","tags":["aws"]}}`,
		"bad service slug": head + index + `,"services":{"sts":{"slug":"STS Coverage","name":"STS","title":"STS",` +
			`"seoTitle":"` + strings.Repeat("x", 39) + ` — Spinifex Docs","description":"` + strings.Repeat("x", 155) + `","tags":["aws"]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParsePageSet([]byte(contents)); err == nil {
				t.Fatal("ParsePageSet accepted unpublishable metadata")
			}
		})
	}
}

func TestRenderServicePagePublishesOnlyImplementedOperations(t *testing.T) {
	t.Parallel()
	pages := testPageSet(t)
	coverage, err := CompareOperations(STS, DispatchInventory{
		Registered:  []string{"AssumeRole", "GetCallerIdentity", "GetSessionToken", "PublishInternal"},
		Stubbed:     []string{"GetSessionToken"},
		Unsupported: []string{"GetCallerIdentity"},
	})
	if err != nil {
		t.Fatal(err)
	}

	page, err := RenderServicePage(coverage, pages, "### Scope\n\nHand-written prose.\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`title: "STS API Coverage"`,
		`category: "Coverage"`,
		"sections:\n  - overview\n",
		"# STS API Coverage\n\n## Overview\n",
		"Spinifex implements **1 operation** in the STS `2011-06-15` API model.",
		"### Scope\n\nHand-written prose.",
		"### Operations\n\n| Operation |\n|---|\n| `AssumeRole` |\n",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page does not contain %q:\n%s", want, page)
		}
	}
	// Everything other than an implemented operation from the pinned model is
	// the internal report's: a stub answers with an error, a refusal serves
	// nothing, and a route the model does not describe has no page to sit on.
	for _, absent := range []string{
		"GetSessionToken", "GetCallerIdentity", "PublishInternal",
		StatusStub, StatusOutsideModel, StatusNotImplemented, "Status",
	} {
		if strings.Contains(page, absent) {
			t.Errorf("page publishes %q:\n%s", absent, page)
		}
	}
	if strings.Contains(page, "<details") || strings.Contains(page, "<summary") {
		t.Errorf("page contains raw HTML the docs site would escape:\n%s", page)
	}
}

// A published page states what the platform serves. An operation Spinifex has
// not implemented is not a row on it, and no count invites reading the page as
// a score against AWS.
func TestRenderServicePagePublishesNeitherGapsNorScores(t *testing.T) {
	t.Parallel()
	pages := testPageSet(t)
	coverage, err := CompareOperations(STS, DispatchInventory{Registered: []string{"AssumeRole"}})
	if err != nil {
		t.Fatal(err)
	}

	page, err := RenderServicePage(coverage, pages, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, StatusNotImplemented) {
		t.Errorf("page publishes an unimplemented operation:\n%s", page)
	}
	if strings.Contains(page, "%") || strings.Contains(page, " of the ") {
		t.Errorf("page publishes a coverage score:\n%s", page)
	}
	for _, operation := range coverage.Missing {
		if strings.Contains(page, "| `"+operation+"` |") {
			t.Errorf("page has a row for unimplemented operation %q", operation)
		}
	}
	for _, operation := range coverage.Implemented {
		if !strings.Contains(page, "| `"+operation+"` |") {
			t.Errorf("page has no row for implemented operation %q", operation)
		}
	}
}

// A service page says what Spinifex serves, so a service that serves nothing
// has no page rather than an empty one.
func TestRenderServicePageRefusesAServiceWithNoImplementation(t *testing.T) {
	t.Parallel()
	pages := testPageSet(t)
	coverage, err := CompareOperations(STS, DispatchInventory{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := RenderServicePage(coverage, pages, ""); err == nil {
		t.Fatal("RenderServicePage published a page for a service with no implemented operations")
	}
}

func TestRenderServicePageWithoutIntro(t *testing.T) {
	t.Parallel()
	pages := testPageSet(t)
	coverage, err := CompareOperations(STS, DispatchInventory{Registered: []string{"AssumeRole"}})
	if err != nil {
		t.Fatal(err)
	}

	page, err := RenderServicePage(coverage, pages, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "\n\n\n") {
		t.Errorf("page has a blank run where the intro would be:\n%s", page)
	}
}

func TestRenderServicePageRejectsIntroOpeningASection(t *testing.T) {
	t.Parallel()
	pages := testPageSet(t)
	coverage, err := CompareOperations(STS, DispatchInventory{Registered: []string{"AssumeRole"}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := RenderServicePage(coverage, pages, "## Troubleshooting\n"); err == nil {
		t.Fatal("RenderServicePage accepted an intro that opens a new page section")
	}
}

// A "never" claim is only worth publishing if it cannot quietly become false.
func TestOperationStatusesRejectsStaleNotApplicableClaims(t *testing.T) {
	t.Parallel()
	coverage, err := CompareOperations(STS, DispatchInventory{Registered: []string{"AssumeRole"}})
	if err != nil {
		t.Fatal(err)
	}

	for name, declared := range map[string]map[string]string{
		"not in the model": {"NotAnOperation": "a reason"},
		"no reason":        {"GetFederationToken": ""},
		"already implemented": {
			"AssumeRole": "claims the platform will never serve an operation it already serves",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := coverage.OperationStatuses(PageMetadata{NotApplicable: declared}); err == nil {
				t.Fatal("OperationStatuses accepted a claim that cannot hold")
			}
		})
	}
}

func TestOperationStatusesCarriesTheNoteKey(t *testing.T) {
	t.Parallel()
	coverage, err := CompareOperations(STS, DispatchInventory{Registered: []string{"AssumeRole"}})
	if err != nil {
		t.Fatal(err)
	}

	statuses, err := coverage.OperationStatuses(PageMetadata{
		NotApplicable: map[string]string{"GetFederationToken": "no-broker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.Operation != "GetFederationToken" {
			continue
		}
		if status.Status != StatusNotApplicable || status.NoteKey != "no-broker" {
			t.Fatalf("status = %q, note key = %q", status.Status, status.NoteKey)
		}
		return
	}
	t.Fatal("declared operation is missing from the table")
}

// Platform plumbing is left out of a page about the AWS API, but only where it
// is declared: an undeclared route stays visible, so a real AWS operation newer
// than the pinned model cannot disappear by being registered.
func TestOperationStatusesHidesDeclaredInternalRoutes(t *testing.T) {
	t.Parallel()
	coverage, err := CompareOperations(STS, DispatchInventory{
		Registered: []string{"AssumeRole", "PublishInternal", "AheadOfThePin"},
	})
	if err != nil {
		t.Fatal(err)
	}

	statuses, err := coverage.OperationStatuses(PageMetadata{Internal: []string{"PublishInternal"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.Operation == "PublishInternal" {
			t.Error("a declared internal route is published")
		}
	}
	if !slices.ContainsFunc(statuses, func(s OperationStatus) bool {
		return s.Operation == "AheadOfThePin" && s.Status == StatusOutsideModel
	}) {
		t.Error("an undeclared route outside the pinned model is not published")
	}
}

func TestOperationStatusesRejectsAnInternalRouteThatIsModelled(t *testing.T) {
	t.Parallel()
	coverage, err := CompareOperations(STS, DispatchInventory{Registered: []string{"AssumeRole"}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := coverage.OperationStatuses(PageMetadata{Internal: []string{"AssumeRole"}}); err == nil {
		t.Fatal("OperationStatuses hid a modelled AWS operation as internal plumbing")
	}
}

// A page says what the platform serves. An operation it will never serve is not
// that, so neither the row nor the reason behind it reaches the page: both are
// the internal report's to carry.
func TestRenderServicePagePublishesNoDeclaredAbsence(t *testing.T) {
	t.Parallel()
	pages := testPageSet(t)
	iam := pages.Services[IAM]
	if len(iam.NotApplicable) == 0 {
		t.Skip("no operations are declared not applicable for IAM")
	}
	coverage, err := CompareOperations(IAM, DispatchInventory{Registered: []string{"CreateUser"}})
	if err != nil {
		t.Fatal(err)
	}

	page, err := RenderServicePage(coverage, pages, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, StatusNotApplicable) || strings.Contains(page, "### Notes") {
		t.Errorf("page publishes a declared absence:\n%s", page)
	}
	for operation := range iam.NotApplicable {
		if strings.Contains(page, "`"+operation+"`") {
			t.Errorf("declared operation %q is published", operation)
		}
	}
	for _, text := range iam.Notes {
		if strings.Contains(page, text) {
			t.Errorf("note %q is published", text)
		}
	}
}

// "Not applicable" says the platform will never serve an operation, which a
// handler refusing it today does not establish. Only the page's declaration
// publishes that claim; an undeclared refusal is a gap like any other.
func TestRenderServicePageDoesNotPublishAnUndeclaredRefusalAsPermanent(t *testing.T) {
	t.Parallel()
	pages := testPageSet(t)
	coverage, err := CompareOperations(STS, DispatchInventory{
		Registered:  []string{"AssumeRole", "GetFederationToken"},
		Unsupported: []string{"GetFederationToken"},
	})
	if err != nil {
		t.Fatal(err)
	}

	page, err := RenderServicePage(coverage, pages, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "| `GetFederationToken` |") {
		t.Errorf("an undeclared refusal is published as a permanent claim:\n%s", page)
	}
}

func TestRenderServicePageRejectsBrokenNoteReferences(t *testing.T) {
	pages := testPageSet(t)
	coverage, err := CompareOperations(STS, DispatchInventory{Registered: []string{"AssumeRole"}})
	if err != nil {
		t.Fatal(err)
	}
	sts := pages.Services[STS]

	for name, mutate := range map[string]func(*PageMetadata){
		"undefined note": func(p *PageMetadata) {
			p.NotApplicable = map[string]string{"GetFederationToken": "nonexistent"}
		},
		"unreferenced note": func(p *PageMetadata) {
			p.Notes = map[string]string{"orphan": "Nothing points at this."}
		},
		"empty note": func(p *PageMetadata) {
			p.Notes = map[string]string{"blank": ""}
			p.NotApplicable = map[string]string{"GetFederationToken": "blank"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			broken := sts
			mutate(&broken)
			pages.Services[STS] = broken
			defer func() { pages.Services[STS] = sts }()

			if _, err := RenderServicePage(coverage, pages, ""); err == nil {
				t.Fatal("RenderServicePage accepted a broken note reference")
			}
		})
	}
}

// An opaque service has no dispatch table to count, so publishing a page for it
// would report 0% where the truth is that the surface is served elsewhere.
func TestRenderServicePageRefusesOpaqueCoverage(t *testing.T) {
	t.Parallel()
	pages := testPageSet(t)
	coverage, err := CompareOperations(STS, DispatchInventory{
		Opaque: true,
		Note:   "Served elsewhere.",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := RenderServicePage(coverage, pages, ""); err == nil {
		t.Fatal("RenderServicePage published a page for a service with no enumerable coverage")
	}
}

// The total counts the pages it links and nothing else, so a service with no
// page of its own is not folded into the headline figure.
func TestRenderIndexPageTotalsOnlyPublishedServices(t *testing.T) {
	t.Parallel()
	pages := testPageSet(t)
	sts, err := CompareOperations(STS, DispatchInventory{Registered: []string{"AssumeRole", "GetSessionToken"}})
	if err != nil {
		t.Fatal(err)
	}
	iam, err := CompareOperations(IAM, DispatchInventory{Registered: []string{"CreateUser"}})
	if err != nil {
		t.Fatal(err)
	}
	// A service the page set does not describe, so the index neither links nor
	// counts it.
	s3, err := CompareOperations(S3, DispatchInventory{Registered: []string{"ListBuckets"}})
	if err != nil {
		t.Fatal(err)
	}
	delete(pages.Services, S3)

	index, err := RenderIndexPage([]OperationCoverage{sts, iam, s3}, pages, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"The platform serves **3 operations** across the AWS APIs below.",
		"| **Total** | **3** |",
	} {
		if !strings.Contains(index, want) {
			t.Errorf("index does not contain %q:\n%s", want, index)
		}
	}
}

func TestRenderIndexPageLinksEveryPublishedService(t *testing.T) {
	t.Parallel()
	pages := testPageSet(t)
	coverages := make([]OperationCoverage, 0, len(Services()))
	for _, service := range Services() {
		coverage, err := CompareOperations(service, DispatchInventory{})
		if err != nil {
			t.Fatal(err)
		}
		coverages = append(coverages, coverage)
	}

	index, err := RenderIndexPage(coverages, pages, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(index, "%") {
		t.Errorf("index publishes a coverage score:\n%s", index)
	}
	if !strings.Contains(index, "| **Total** |") {
		t.Errorf("index does not total the operations it lists:\n%s", index)
	}
	for _, service := range Services() {
		page, published := pages.Services[service]
		if !published {
			if strings.Contains(index, pages.BasePath+"/"+string(service)) {
				t.Errorf("index links unpublished service %q", service)
			}
			continue
		}
		if !strings.Contains(index, "["+page.Name+"]("+pages.BasePath+"/"+page.Slug+")") {
			t.Errorf("index does not link %q at %s/%s", page.Name, pages.BasePath, page.Slug)
		}
	}
}
