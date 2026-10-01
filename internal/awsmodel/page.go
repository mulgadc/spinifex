package awsmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Operation states. Only Implemented reaches a published page; the rest sort
// the internal report. The symbol carries the status at a glance and the label
// disambiguates it; the docs site escapes inline SVG, so these are runes.
const (
	StatusImplemented    = "✅ Implemented"
	StatusStub           = "🟡 Stub"
	StatusNotApplicable  = "⛔ Not applicable"
	StatusNotImplemented = "❌ Not implemented"
	StatusOutsideModel   = "🔒 Outside the pinned model"
)

// Frontmatter length rules enforced by the docs site. See docs/README.md.
const (
	seoTitleSuffix = " — Spinifex Docs"
	seoTitleMin    = 50
	seoTitleMax    = 60
	descriptionMin = 150
	descriptionMax = 160
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// OperationStatus is one row of a published coverage table. NoteKey names the
// shared explanation behind the row's status, where one is declared.
type OperationStatus struct {
	Operation string
	Status    string
	NoteKey   string
}

// ImplementedPercent is the share of modelled operations bound to a real
// handler, or zero for a model with no operations.
func (c OperationCoverage) ImplementedPercent() float64 {
	if len(c.Modelled) == 0 {
		return 0
	}
	return 100 * float64(len(c.Implemented)) / float64(len(c.Modelled))
}

// OperationStatuses returns every modelled operation with its dispatch state,
// followed by the registered operations the pinned model does not describe,
// less the internal routes the page declares.
//
// The page's NotApplicable declares operations the platform will never serve,
// mapped to the note key explaining why. Nothing published carries that claim:
// it separates a permanent absence from candidate work in the internal report.
// A handler that refuses today may be implemented tomorrow, so a refusal on its
// own is a gap like any other rather than a "never".
func (c OperationCoverage) OperationStatuses(page PageMetadata) ([]OperationStatus, error) {
	registered := toSet(c.Registered)
	stubbed := toSet(c.Stubbed)
	unsupported := toSet(c.Unsupported)
	modelled := toSet(c.Modelled)
	extra := toSet(c.Extra)
	notApplicable := page.NotApplicable

	for operation, noteKey := range notApplicable {
		switch {
		case noteKey == "":
			return nil, fmt.Errorf("awsmodel: %s operation %q is declared not applicable with no note", c.Service, operation)
		case !modelled[operation]:
			return nil, fmt.Errorf("awsmodel: %s operation %q is declared not applicable but is not in the pinned model", c.Service, operation)
		case registered[operation] && !unsupported[operation]:
			return nil, fmt.Errorf("awsmodel: %s operation %q is declared not applicable but is registered to a handler", c.Service, operation)
		}
	}

	internal := map[string]bool{}
	for _, operation := range page.Internal {
		if !extra[operation] {
			return nil, fmt.Errorf("awsmodel: %s route %q is declared internal but is not a registered route outside the pinned model", c.Service, operation)
		}
		internal[operation] = true
	}

	statuses := make([]OperationStatus, 0, len(c.Modelled)+len(c.Extra))
	for _, operation := range c.Modelled {
		status := StatusNotImplemented
		switch {
		case stubbed[operation]:
			status = StatusStub
		// A registered handler that refuses serves nothing, so it reads as the
		// gap it is unless the page declares why it is permanent.
		case unsupported[operation]:
			status = StatusNotImplemented
		case registered[operation]:
			status = StatusImplemented
		}
		if noteKey, ok := notApplicable[operation]; ok {
			statuses = append(statuses, OperationStatus{Operation: operation, Status: StatusNotApplicable, NoteKey: noteKey})
			continue
		}
		statuses = append(statuses, OperationStatus{Operation: operation, Status: status})
	}
	// A route outside the pinned model is still only as real as its handler, so
	// its dispatch state wins: being absent from the model says nothing about
	// whether the platform answers it.
	for _, operation := range c.Extra {
		if internal[operation] {
			continue
		}
		status := StatusOutsideModel
		switch {
		case stubbed[operation]:
			status = StatusStub
		case unsupported[operation]:
			status = StatusNotImplemented
		}
		statuses = append(statuses, OperationStatus{Operation: operation, Status: status})
	}
	return statuses, nil
}

// PageMetadata is the checked-in frontmatter for one published coverage page.
// It is hand-maintained rather than derived, because a description carrying a
// live count would drift outside the length the docs site enforces.
type PageMetadata struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Title       string   `json:"title"`
	SEOTitle    string   `json:"seoTitle"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`

	// ImplementedBy names the component that serves the surface, where it is
	// not Spinifex itself. Predastore serves S3, and the page says so.
	ImplementedBy string `json:"implementedBy,omitempty"`

	// Notes are the shared explanations a page footnotes, keyed by a short
	// name. One note covers every operation that shares a reason.
	Notes map[string]string `json:"notes,omitempty"`

	// NotApplicable maps an operation the platform will never serve to the key
	// of the note explaining why. Checked against the model, the notes and the
	// dispatch tables at render, so a typo or a later implementation fails the
	// build rather than publishing a claim that has quietly become false.
	NotApplicable map[string]string `json:"notApplicable,omitempty"`

	// Internal names registered routes that carry platform plumbing rather than
	// a tenant-callable AWS action, which the page leaves out. A route outside
	// the pinned model that is not declared here is published, so a real AWS
	// operation newer than the pin surfaces instead of disappearing quietly.
	Internal []string `json:"internal,omitempty"`
}

// PageSet is the metadata for the index page and every service page. BasePath
// is the site section the pages are published under; the index answers it, so
// the index alone carries no slug.
type PageSet struct {
	Category string                   `json:"category"`
	BasePath string                   `json:"basePath"`
	Index    PageMetadata             `json:"index"`
	Services map[Service]PageMetadata `json:"services"`
}

// ParsePageSet decodes docs/compatibility/pages.json and checks it describes
// exactly the services the loader knows about, with publishable frontmatter.
func ParsePageSet(contents []byte) (PageSet, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var pages PageSet
	if err := decoder.Decode(&pages); err != nil {
		return PageSet{}, fmt.Errorf("awsmodel: parse coverage pages: %w", err)
	}
	if pages.Category == "" {
		return PageSet{}, fmt.Errorf("awsmodel: coverage pages: category is empty")
	}
	if !strings.HasPrefix(pages.BasePath, "/") {
		return PageSet{}, fmt.Errorf("awsmodel: coverage pages: basePath %q is not a site path", pages.BasePath)
	}

	slugs := map[string]bool{}
	if err := validateIndexPage(pages.Index); err != nil {
		return PageSet{}, err
	}
	// A known service may be left out. S3 is: Predastore serves that surface,
	// so the gateway dispatch tables hold no honest number to publish for it.
	for _, service := range Services() {
		page, ok := pages.Services[service]
		if !ok {
			continue
		}
		if err := validatePage(string(service), page, slugs); err != nil {
			return PageSet{}, err
		}
	}
	for service := range pages.Services {
		if _, ok := modelFiles[service]; !ok {
			return PageSet{}, fmt.Errorf("awsmodel: coverage pages: unknown service %q", service)
		}
	}
	return pages, nil
}

// The index answers the section's base path, so it has no slug of its own.
func validateIndexPage(page PageMetadata) error {
	if page.Slug != "" {
		return fmt.Errorf("awsmodel: coverage page index: slug %q is set, but the index answers the base path", page.Slug)
	}
	return validateFrontmatter("index", page)
}

func validatePage(owner string, page PageMetadata, slugs map[string]bool) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("awsmodel: coverage page %s: %s", owner, fmt.Sprintf(format, args...))
	}
	if !slugPattern.MatchString(page.Slug) {
		return fail("slug %q is not lowercase kebab-case", page.Slug)
	}
	if slugs[page.Slug] {
		return fail("slug %q is used by another page", page.Slug)
	}
	slugs[page.Slug] = true
	return validateFrontmatter(owner, page)
}

func validateFrontmatter(owner string, page PageMetadata) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("awsmodel: coverage page %s: %s", owner, fmt.Sprintf(format, args...))
	}
	if page.Name == "" || page.Title == "" {
		return fail("name and title are required")
	}
	if len(page.Tags) == 0 {
		return fail("at least one tag is required")
	}
	if !strings.HasSuffix(page.SEOTitle, seoTitleSuffix) {
		return fail("seoTitle %q does not end with %q", page.SEOTitle, seoTitleSuffix)
	}
	if length := utf8.RuneCountInString(page.SEOTitle); length < seoTitleMin || length > seoTitleMax {
		return fail("seoTitle is %d characters, want %d-%d", length, seoTitleMin, seoTitleMax)
	}
	if length := utf8.RuneCountInString(page.Description); length < descriptionMin || length > descriptionMax {
		return fail("description is %d characters, want %d-%d", length, descriptionMin, descriptionMax)
	}
	return nil
}

// noteOrder returns the note keys in the order they are published. Sorted by
// key rather than by first use, so adding an operation cannot renumber a note
// that is already published.
func noteOrder(notes map[string]string) []string {
	return slices.Sorted(maps.Keys(notes))
}

// checkNotes rejects a note nothing references and a reference to a note that
// does not exist. The notes reach the internal report rather than a page, and
// the check keeps them honest wherever they are read.
func checkNotes(page PageMetadata) error {
	referenced := map[string]bool{}
	for operation, key := range page.NotApplicable {
		if _, ok := page.Notes[key]; !ok {
			return fmt.Errorf("awsmodel: coverage page %s: operation %q references undefined note %q", page.Slug, operation, key)
		}
		referenced[key] = true
	}

	for _, key := range noteOrder(page.Notes) {
		if !referenced[key] {
			return fmt.Errorf("awsmodel: coverage page %s: note %q is never referenced", page.Slug, key)
		}
		if page.Notes[key] == "" {
			return fmt.Errorf("awsmodel: coverage page %s: note %q is empty", page.Slug, key)
		}
	}
	return nil
}

// RenderServicePage renders one publishable coverage page. intro is optional
// hand-written prose emitted above the table; it may not open a new page
// section, so its headings must be level three or deeper.
func RenderServicePage(coverage OperationCoverage, pages PageSet, intro string) (string, error) {
	if err := checkIntro(string(coverage.Service), intro); err != nil {
		return "", err
	}
	page, ok := pages.Services[coverage.Service]
	if !ok {
		return "", fmt.Errorf("awsmodel: coverage pages: no entry for service %q", coverage.Service)
	}
	// An opaque service has no dispatch table to count, so there is no honest
	// number to put on a page. It is left out until a real one exists.
	if coverage.Opaque {
		return "", fmt.Errorf("awsmodel: %s coverage is not enumerable, so it has no publishable page", coverage.Service)
	}

	// A page exists to say what the platform serves. A service with nothing
	// behind it has no such statement to make.
	if len(coverage.Implemented) == 0 {
		return "", fmt.Errorf("awsmodel: %s has no implemented operations, so it has no publishable page", coverage.Service)
	}

	statuses, err := coverage.OperationStatuses(page)
	if err != nil {
		return "", err
	}
	if err := checkNotes(page); err != nil {
		return "", err
	}

	var body strings.Builder
	writeFrontmatter(&body, page, pages.Category)
	fmt.Fprintf(&body, "# %s\n\n## Overview\n\n", page.Title)

	noun := "operations"
	if len(coverage.Implemented) == 1 {
		noun = "operation"
	}
	implementer := page.ImplementedBy
	if implementer == "" {
		implementer = "Spinifex"
	}
	fmt.Fprintf(&body, "%s implements **%d %s** in the %s `%s` API model.\n\n",
		implementer, len(coverage.Implemented), noun, page.Name, coverage.APIVersion)

	if intro != "" {
		body.WriteString(strings.TrimSpace(intro) + "\n\n")
	}

	writeOperations(&body, statuses)
	return body.String(), nil
}

// writeOperations publishes one row per operation the platform implements from
// the pinned model, and nothing else. There is no status column because every
// row carries the same status: a gap, a stub, a route the model does not
// describe and an operation the platform will never offer are all the internal
// report's to carry.
func writeOperations(body *strings.Builder, statuses []OperationStatus) {
	body.WriteString("### Operations\n\n| Operation |\n|---|\n")
	for _, status := range statuses {
		if status.Status != StatusImplemented {
			continue
		}
		fmt.Fprintf(body, "| `%s` |\n", status.Operation)
	}
}

// RenderIndexPage renders the cross-service summary that links to each page.
func RenderIndexPage(coverages []OperationCoverage, pages PageSet, intro string) (string, error) {
	if err := checkIntro("index", intro); err != nil {
		return "", err
	}

	var body strings.Builder
	writeFrontmatter(&body, pages.Index, pages.Category)
	fmt.Fprintf(&body, "# %s\n\n## Overview\n\n", pages.Index.Title)
	total := 0
	for _, coverage := range coverages {
		if _, ok := pages.Services[coverage.Service]; ok {
			total += len(coverage.Implemented)
		}
	}

	fmt.Fprintf(&body, "The platform serves **%d operations** across the AWS APIs below. Every page names the operations its service implements from the pinned model, generated from the dispatch tables on each build rather than written by hand.\n\n", total)
	body.WriteString("| Service | Operations |\n|---|---:|\n")
	for _, coverage := range sortedCoverages(coverages) {
		page, ok := pages.Services[coverage.Service]
		if !ok {
			continue
		}
		fmt.Fprintf(&body, "| [%s](%s/%s) | %d |\n",
			page.Name, pages.BasePath, page.Slug, len(coverage.Implemented))
	}
	fmt.Fprintf(&body, "| **Total** | **%d** |\n", total)

	if intro != "" {
		body.WriteString("\n" + strings.TrimSpace(intro) + "\n")
	}
	return body.String(), nil
}

func writeFrontmatter(body *strings.Builder, page PageMetadata, category string) {
	body.WriteString("---\n")
	fmt.Fprintf(body, "title: %s\n", strconv.Quote(page.Title))
	fmt.Fprintf(body, "seoTitle: %s\n", strconv.Quote(page.SEOTitle))
	fmt.Fprintf(body, "description: %s\n", strconv.Quote(page.Description))
	fmt.Fprintf(body, "category: %s\n", strconv.Quote(category))
	body.WriteString("sections:\n  - overview\ntags:\n")
	for _, tag := range page.Tags {
		fmt.Fprintf(body, "  - %s\n", tag)
	}
	body.WriteString("---\n\n")
}

func checkIntro(owner, intro string) error {
	for line := range strings.SplitSeq(intro, "\n") {
		if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") {
			return fmt.Errorf("awsmodel: %s intro heading %q opens a new page section; use level three or deeper", owner, strings.TrimSpace(line))
		}
	}
	return nil
}
