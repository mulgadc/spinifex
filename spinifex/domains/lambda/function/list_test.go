package function

import (
	"fmt"
	"math"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listPage(t *testing.T, svc *MetaService, region, marker string, maxItems int) *ListResponse {
	t.Helper()
	resp, err := svc.List(t.Context(), &ListRequest{Region: region, Marker: marker, MaxItems: maxItems}, testAccount)
	require.NoError(t, err)
	return resp
}

// walk follows NextMarker to the end and returns the names in order and the
// number of pages.
func walk(t *testing.T, svc *MetaService, region string, maxItems int) ([]string, int) {
	t.Helper()
	var names []string
	marker, pages := "", 0
	for {
		resp := listPage(t, svc, region, marker, maxItems)
		pages++
		require.LessOrEqual(t, len(resp.Functions), maxItems)
		for _, f := range resp.Functions {
			names = append(names, f.Record.FunctionName)
		}
		if resp.NextMarker == "" {
			return names, pages
		}
		marker = resp.NextMarker
		require.Less(t, pages, 1000, "the walk does not end")
	}
}

func TestList_EmptyAccountHasNoFunctionsAndNoMarker(t *testing.T) {
	svc, _ := newTestService(t)
	resp := listPage(t, svc, testRegion, "", MaxListPage)
	assert.Empty(t, resp.Functions)
	assert.NotNil(t, resp.Functions)
	assert.Empty(t, resp.NextMarker)
}

func TestList_PagesReturnEveryFunctionOnceWithItsRevision(t *testing.T) {
	svc, _ := newTestService(t)
	want := make([]string, 0, 7)
	for i := range 7 {
		name := fmt.Sprintf("fn-%d", i)
		mustCreate(t, svc, name, fmt.Sprintf("f%d", i))
		want = append(want, name)
	}
	names, pages := walk(t, svc, testRegion, 2)
	assert.Equal(t, want, names, "stream order, each once")
	assert.Equal(t, 4, pages)

	page := listPage(t, svc, testRegion, "", 1)
	require.Len(t, page.Functions, 1)
	got := mustGet(t, svc, "fn-0")
	assert.Equal(t, got.Revision, page.Functions[0].Revision)
	assert.Equal(t, got.Record, page.Functions[0].Record)
}

// A page that fills on the last function looks ahead, finds the walk over,
// and returns no marker; one that fills before it keeps the next one.
func TestList_FullPageAtTheEndHasNoMarker(t *testing.T) {
	svc, _ := newTestService(t)
	for i := range 4 {
		mustCreate(t, svc, fmt.Sprintf("fn-%d", i), fmt.Sprintf("f%d", i))
	}
	_, pages := walk(t, svc, testRegion, 2)
	assert.Equal(t, 2, pages)
	assert.Empty(t, listPage(t, svc, testRegion, "", 4).NextMarker)

	first := listPage(t, svc, testRegion, "", 3)
	require.NotEmpty(t, first.NextMarker)
	rest := listPage(t, svc, testRegion, first.NextMarker, 3)
	require.Len(t, rest.Functions, 1, "the record the full page looked at is not skipped")
	assert.Equal(t, "fn-3", rest.Functions[0].Record.FunctionName)
	assert.Empty(t, rest.NextMarker)
}

func TestList_SkipsDeletedFunctionsUploadsObligationsAndOtherRegions(t *testing.T) {
	svc, _ := newTestService(t)
	gone := serveAs(t, svc, "gone", "f0")
	mustCreate(t, svc, "kept", "f1")
	_, err := svc.Delete(t.Context(), deleteReq(gone), testAccount)
	require.NoError(t, err)
	other := createReq("elsewhere", "f2")
	other.Region = "us-east-1"
	_, err = svc.Create(t.Context(), other, testAccount)
	require.NoError(t, err)

	names, _ := walk(t, svc, testRegion, MaxListPage)
	assert.Equal(t, []string{"kept"}, names)
	names, _ = walk(t, svc, "us-east-1", MaxListPage)
	assert.Equal(t, []string{"elsewhere"}, names)
}

// cursorAfter is the last sequence a marker says its page examined.
func cursorAfter(t *testing.T, marker string) uint64 {
	t.Helper()
	after, err := decodeCursor(marker, testAccount, testRegion)
	require.NoError(t, err)
	return after
}

// walkFrom follows NextMarker from marker to the end, checking that no page
// examines more than maxListScan sequences, and returns the names it listed.
func walkFrom(t *testing.T, svc *MetaService, marker string, maxItems int) ([]string, int) {
	t.Helper()
	var names []string
	var after uint64
	if marker != "" {
		after = cursorAfter(t, marker)
	}
	for pages := 1; ; pages++ {
		resp := listPage(t, svc, testRegion, marker, maxItems)
		require.LessOrEqual(t, len(resp.Functions), maxItems)
		for _, f := range resp.Functions {
			names = append(names, f.Record.FunctionName)
		}
		if resp.NextMarker == "" {
			return names, pages
		}
		next := cursorAfter(t, resp.NextMarker)
		require.LessOrEqual(t, next-after, uint64(maxListScan), "a page examined more than its scan bound")
		require.True(t, next > after || len(resp.Functions) > 0, "a page made no progress")
		marker, after = resp.NextMarker, next
		require.Less(t, pages, 1000, "the walk does not end")
	}
}

// tombstones appends n deleted functions to the stream.
func tombstones(t *testing.T, svc *MetaService, n int) {
	t.Helper()
	kv, err := svc.bucket(t.Context(), testAccount)
	require.NoError(t, err)
	for i := range n {
		name := fmt.Sprintf("tomb-%d", i)
		mustCreate(t, svc, name, fmt.Sprintf("t%d", i))
		require.NoError(t, kv.Delete(t.Context(), KVFunctionKey(testRegion, name)))
	}
}

// More delete markers than one request may examine give empty pages that
// still continue, and the walk reaches the live function after them.
func TestList_ScanBoundGivesShortPagesThatContinue(t *testing.T) {
	svc, _ := newTestService(t)
	tombstones(t, svc, maxListScan+5)
	mustCreate(t, svc, "live", "f1")

	first := listPage(t, svc, testRegion, "", MaxListPage)
	assert.Empty(t, first.Functions)
	require.NotEmpty(t, first.NextMarker)
	assert.Equal(t, uint64(maxListScan), cursorAfter(t, first.NextMarker))
	names, _ := walkFrom(t, svc, first.NextMarker, MaxListPage)
	assert.Equal(t, []string{"live"}, names)
}

// Messages that are not this Region's function records count toward the scan
// bound like any other sequence: uploads, obligations, other keys and other
// Regions' functions.
func TestList_ExcludedRecordsCountTowardTheScanBound(t *testing.T) {
	svc, _ := newTestService(t)
	mustCreate(t, svc, "first", "f1")
	kv, err := svc.bucket(t.Context(), testAccount)
	require.NoError(t, err)
	for i := range maxListScan + 30 {
		var key string
		switch i % 4 {
		case 0:
			key = KVFunctionKey("us-east-1", fmt.Sprintf("x-%d", i))
		case 1:
			key = fmt.Sprintf("upload.%s.f9.%d", testRegion, i)
		case 2:
			key = KVDeletedKey(testRegion, fmt.Sprintf("d%d", i))
		default:
			key = fmt.Sprintf("other.%d", i)
		}
		_, err := kv.Put(t.Context(), key, []byte("{}"))
		require.NoError(t, err)
	}
	mustCreate(t, svc, "last", "f2")

	names, pages := walkFrom(t, svc, "", MaxListPage)
	assert.Equal(t, []string{"first", "last"}, names)
	assert.GreaterOrEqual(t, pages, 2)
}

// A key rewritten many times leaves gaps behind its latest revision; missing
// sequences count toward the bound and the walk crosses them.
func TestList_SparseSequencesCountTowardTheScanBound(t *testing.T) {
	svc, _ := newTestService(t)
	mustCreate(t, svc, "first", "f1")
	kv, err := svc.bucket(t.Context(), testAccount)
	require.NoError(t, err)
	for range 2*maxListScan + 7 {
		_, err := kv.Put(t.Context(), "scratch", []byte("{}"))
		require.NoError(t, err)
	}
	mustCreate(t, svc, "middle", "f2")
	for range maxListScan {
		_, err := kv.Put(t.Context(), "scratch", []byte("{}"))
		require.NoError(t, err)
	}
	mustCreate(t, svc, "last", "f3")

	page := listPage(t, svc, testRegion, "", MaxListPage)
	require.Len(t, page.Functions, 1)
	assert.Equal(t, uint64(maxListScan), cursorAfter(t, page.NextMarker))
	for _, size := range []int{1, MaxListPage} {
		names, _ := walkFrom(t, svc, "", size)
		assert.Equal(t, []string{"first", "middle", "last"}, names, "page size %d", size)
	}
}

// A large live set is returned once each, in stream order, in pages that
// never exceed the requested size.
func TestList_LargeLiveSetPagesEveryFunctionOnce(t *testing.T) {
	svc, _ := newTestService(t)
	want := make([]string, 0, 3*MaxListPage+7)
	for i := range cap(want) {
		name := fmt.Sprintf("fn-%03d", i)
		mustCreate(t, svc, name, fmt.Sprintf("f%d", i))
		want = append(want, name)
	}
	for _, size := range []int{1, 7, MaxListPage} {
		names, _ := walkFrom(t, svc, "", size)
		assert.Equal(t, want, names, "page size %d", size)
	}
}

// A full page looking ahead through delete markers to the end of the stream
// returns no marker; when the scan bound runs out first, it continues and a
// later page finds the function beyond them.
func TestList_LookAheadThroughDeleteMarkers(t *testing.T) {
	t.Run("reaches the end", func(t *testing.T) {
		svc, _ := newTestService(t)
		mustCreate(t, svc, "live", "f1")
		tombstones(t, svc, 3)
		page := listPage(t, svc, testRegion, "", 1)
		require.Len(t, page.Functions, 1)
		assert.Empty(t, page.NextMarker)
	})
	t.Run("scan bound exhausted", func(t *testing.T) {
		svc, _ := newTestService(t)
		mustCreate(t, svc, "live", "f1")
		tombstones(t, svc, maxListScan)
		mustCreate(t, svc, "beyond", "f2")
		page := listPage(t, svc, testRegion, "", 1)
		require.Len(t, page.Functions, 1)
		assert.Equal(t, "live", page.Functions[0].Record.FunctionName)
		require.NotEmpty(t, page.NextMarker, "the bound ran out before the walk was known to end")
		names, _ := walkFrom(t, svc, page.NextMarker, 1)
		assert.Equal(t, []string{"beyond"}, names)
	})
}

// An update moves a function to a later sequence, so this walk, begun before
// the update, meets it again after the function it had not yet reached.
func TestList_UpdateDuringWalkMovesFunctionLater(t *testing.T) {
	svc, _ := newTestService(t)
	first := serveAs(t, svc, "a", "f1")
	serveAs(t, svc, "b", "f2")
	page := listPage(t, svc, testRegion, "", 1)
	require.Equal(t, "a", page.Functions[0].Record.FunctionName)
	cfg := Configuration{Role: first.Record.Role, Handler: "handler.two", Timeout: 3, MemorySize: 128}
	_, err := svc.UpdateConfiguration(t.Context(), &ConfigurationUpdatedRequest{Region: testRegion, FunctionName: "a",
		FunctionID: "f1", Revision: first.Revision, Configuration: cfg}, testAccount)
	require.NoError(t, err)

	var rest []string
	for marker := page.NextMarker; marker != ""; {
		p := listPage(t, svc, testRegion, marker, 1)
		for _, f := range p.Functions {
			rest = append(rest, f.Record.FunctionName)
		}
		marker = p.NextMarker
	}
	assert.Equal(t, []string{"b", "a"}, rest)
}

func TestList_RefusesForeignMalformedAndExhaustedMarkers(t *testing.T) {
	svc, _ := newTestService(t)
	for i := range 3 {
		mustCreate(t, svc, fmt.Sprintf("fn-%d", i), fmt.Sprintf("f%d", i))
	}
	marker := listPage(t, svc, testRegion, "", 1).NextMarker
	require.NotEmpty(t, marker)

	foreign, err := svc.List(t.Context(), &ListRequest{Region: testRegion, Marker: marker, MaxItems: 1}, "000000000002")
	assert.Nil(t, foreign)
	testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
	_, err = svc.List(t.Context(), &ListRequest{Region: "us-east-1", Marker: marker, MaxItems: 1}, testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)

	exhausted, err := encodeCursor(listCursor{Version: 1, AccountID: testAccount, Region: testRegion, After: math.MaxUint64})
	require.NoError(t, err)
	zero, err := encodeCursor(listCursor{Version: 1, AccountID: testAccount, Region: testRegion})
	require.NoError(t, err)
	future, err := encodeCursor(listCursor{Version: 2, AccountID: testAccount, Region: testRegion, After: 1})
	require.NoError(t, err)
	for _, bad := range []string{"not base64!", "e30", exhausted, zero, future} {
		_, err := svc.List(t.Context(), &ListRequest{Region: testRegion, Marker: bad, MaxItems: 1}, testAccount)
		testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
	}
}

func TestList_RefusesMalformedRequests(t *testing.T) {
	svc, _ := newTestService(t)
	for _, req := range []*ListRequest{
		{Region: "Bad.Region", MaxItems: 1},
		{Region: testRegion, MaxItems: 0},
		{Region: testRegion, MaxItems: MaxListPage + 1},
	} {
		_, err := svc.List(t.Context(), req, testAccount)
		testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
	}
}
