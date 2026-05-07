package api

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/identity"
)

// fakeCall is an in-test CallView implementation. We do not import
// internal/bridge here on purpose — internal/api ships as a leaf package
// decoupled from bridge. The real *bridge.CallSession satisfies CallView
// via Go's structural typing at the router wire-up site.
type fakeCall struct {
	callSid       string
	accountSid    string
	from          string
	to            string
	direction     string
	status        string
	startTime     time.Time
	endTime       time.Time
	duration      int
	answeredBy    string
	parentCallSid string
}

func (f fakeCall) CallSid() string       { return f.callSid }
func (f fakeCall) AccountSid() string    { return f.accountSid }
func (f fakeCall) From() string          { return f.from }
func (f fakeCall) To() string            { return f.to }
func (f fakeCall) Direction() string     { return f.direction }
func (f fakeCall) Status() string        { return f.status }
func (f fakeCall) StartTime() time.Time  { return f.startTime }
func (f fakeCall) EndTime() time.Time    { return f.endTime }
func (f fakeCall) Duration() int         { return f.duration }
func (f fakeCall) AnsweredBy() string    { return f.answeredBy }
func (f fakeCall) ParentCallSid() string { return f.parentCallSid }

const (
	testAccountSid = "ACdeadbeefdeadbeefdeadbeefdeadbeef"
	testCallSid    = "CAfedcba9876543210fedcba9876543210"
	testPathPrefix = "/2010-04-01/Accounts/" + testAccountSid
)

// TestRFC2822_Format covers Twilio's exact wire format across UTC and non-UTC
// inputs (always normalized to "+0000"). Five fixtures + a non-UTC tz proof.
func TestRFC2822_Format(t *testing.T) {
	t.Parallel()

	utc := time.UTC
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"epoch", time.Date(1970, time.January, 1, 0, 0, 0, 0, utc), "Thu, 01 Jan 1970 00:00:00 +0000"},
		{"plan_example", time.Date(2026, time.April, 27, 10, 0, 0, 0, utc), "Mon, 27 Apr 2026 10:00:00 +0000"},
		{"midnight_dec_31", time.Date(2026, time.December, 31, 23, 59, 59, 0, utc), "Thu, 31 Dec 2026 23:59:59 +0000"},
		{"february_leap", time.Date(2024, time.February, 29, 12, 0, 0, 0, utc), "Thu, 29 Feb 2024 12:00:00 +0000"},
		{"single_digit_day", time.Date(2026, time.January, 5, 8, 30, 0, 0, utc), "Mon, 05 Jan 2026 08:30:00 +0000"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RFC2822(tc.in); got != tc.want {
				t.Fatalf("RFC2822(%v): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Non-UTC input must be normalized to UTC "+0000".
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("Europe/Berlin unavailable: %v", err)
	}
	in := time.Date(2026, time.April, 27, 12, 0, 0, 0, berlin) // == 10:00 UTC in DST
	got := RFC2822(in)
	if !strings.HasSuffix(got, "+0000") {
		t.Fatalf("non-UTC normalization failed: got %q, want suffix +0000", got)
	}
	if got != "Mon, 27 Apr 2026 10:00:00 +0000" {
		t.Fatalf("Berlin DST normalization: got %q, want %q",
			got, "Mon, 27 Apr 2026 10:00:00 +0000")
	}
}

// TestRFC2822_Zero proves the zero time becomes "" (callers turn this into
// JSON null via the *string field).
func TestRFC2822_Zero(t *testing.T) {
	t.Parallel()
	if got := RFC2822(time.Time{}); got != "" {
		t.Fatalf("RFC2822(zero): got %q, want \"\"", got)
	}
}

// TestSerializeCall_ActiveCall: in-progress call has start_time set but
// end_time, duration, answered_by, parent_call_sid all nil.
func TestSerializeCall_ActiveCall(t *testing.T) {
	t.Parallel()

	c := fakeCall{
		callSid:    testCallSid,
		accountSid: testAccountSid,
		from:       "+4915123456789",
		to:         "+4930555555",
		direction:  "inbound",
		status:     "in-progress",
		startTime:  time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC),
	}
	cj := SerializeCall(c, testPathPrefix)

	if cj.Sid != testCallSid {
		t.Fatalf("Sid: got %q, want %q", cj.Sid, testCallSid)
	}
	if cj.AccountSid != testAccountSid {
		t.Fatalf("AccountSid: got %q, want %q", cj.AccountSid, testAccountSid)
	}
	if cj.From != "+4915123456789" || cj.To != "+4930555555" {
		t.Fatalf("From/To mismatch: %q/%q", cj.From, cj.To)
	}
	if cj.Status != "in-progress" || cj.Direction != "inbound" {
		t.Fatalf("Status/Direction: %q/%q", cj.Status, cj.Direction)
	}
	if cj.APIVersion != "2010-04-01" {
		t.Fatalf("APIVersion: got %q, want 2010-04-01", cj.APIVersion)
	}
	if cj.StartTime == nil || *cj.StartTime != "Mon, 27 Apr 2026 10:00:00 +0000" {
		t.Fatalf("StartTime: got %v, want Mon, 27 Apr 2026 10:00:00 +0000", cj.StartTime)
	}
	if cj.EndTime != nil {
		t.Fatalf("EndTime: got %v, want nil", *cj.EndTime)
	}
	if cj.Duration != nil {
		t.Fatalf("Duration: got %v, want nil", *cj.Duration)
	}
	if cj.AnsweredBy != nil {
		t.Fatalf("AnsweredBy: got %v, want nil", *cj.AnsweredBy)
	}
	if cj.ParentCallSid != nil {
		t.Fatalf("ParentCallSid: got %v, want nil", *cj.ParentCallSid)
	}
}

// TestSerializeCall_CompletedCall: end_time set → end_time + duration both
// non-nil; status reflects completion.
func TestSerializeCall_CompletedCall(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC)
	end := start.Add(42 * time.Second)
	c := fakeCall{
		callSid:    testCallSid,
		accountSid: testAccountSid,
		from:       "+4915123456789",
		to:         "+4930555555",
		direction:  "inbound",
		status:     "completed",
		startTime:  start,
		endTime:    end,
		duration:   42,
		answeredBy: "human",
	}
	cj := SerializeCall(c, testPathPrefix)

	if cj.EndTime == nil {
		t.Fatal("EndTime: got nil, want non-nil")
	}
	if *cj.EndTime != "Mon, 27 Apr 2026 10:00:42 +0000" {
		t.Fatalf("EndTime: got %q, want Mon, 27 Apr 2026 10:00:42 +0000", *cj.EndTime)
	}
	if cj.Duration == nil || *cj.Duration != 42 {
		t.Fatalf("Duration: got %v, want 42", cj.Duration)
	}
	if cj.AnsweredBy == nil || *cj.AnsweredBy != "human" {
		t.Fatalf("AnsweredBy: got %v, want human", cj.AnsweredBy)
	}
	if cj.Status != "completed" {
		t.Fatalf("Status: got %q, want completed", cj.Status)
	}
}

// TestSerializeCall_URIShape verifies the canonical Twilio URI pattern using
// the real CallSid/AccountSid regexps from internal/identity.
func TestSerializeCall_URIShape(t *testing.T) {
	t.Parallel()

	c := fakeCall{
		callSid:    testCallSid,
		accountSid: testAccountSid,
		startTime:  time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC),
	}
	cj := SerializeCall(c, testPathPrefix)

	uriRE := regexp.MustCompile(`^/2010-04-01/Accounts/AC[0-9a-f]{32}/Calls/CA[0-9a-f]{32}\.json$`)
	if !uriRE.MatchString(cj.URI) {
		t.Fatalf("URI %q does not match %s", cj.URI, uriRE)
	}
	// Cross-check that the SIDs embedded in the URI match the identity-package
	// regexps — guards against typos in the format string.
	if !identity.AccountSidRE.MatchString(testAccountSid) {
		t.Fatal("test fixture testAccountSid does not match AccountSidRE")
	}
	if !identity.CallSidRE.MatchString(testCallSid) {
		t.Fatal("test fixture testCallSid does not match CallSidRE")
	}
}

// TestSerializeCall_SubresourceURIs verifies all 4 keys are present and each
// follows the canonical pattern.
func TestSerializeCall_SubresourceURIs(t *testing.T) {
	t.Parallel()

	c := fakeCall{
		callSid:    testCallSid,
		accountSid: testAccountSid,
		startTime:  time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC),
	}
	cj := SerializeCall(c, testPathPrefix)

	wantKeys := map[string]string{
		"notifications": testPathPrefix + "/Calls/" + testCallSid + "/Notifications.json",
		"recordings":    testPathPrefix + "/Calls/" + testCallSid + "/Recordings.json",
		"events":        testPathPrefix + "/Calls/" + testCallSid + "/Events.json",
		"siprec":        testPathPrefix + "/Calls/" + testCallSid + "/Siprec.json",
	}
	if len(cj.SubresourceURIs) != len(wantKeys) {
		t.Fatalf("SubresourceURIs length: got %d, want %d (keys: %v)",
			len(cj.SubresourceURIs), len(wantKeys), cj.SubresourceURIs)
	}
	for k, want := range wantKeys {
		got, ok := cj.SubresourceURIs[k]
		if !ok {
			t.Fatalf("missing subresource %q", k)
		}
		if got != want {
			t.Fatalf("subresource %q: got %q, want %q", k, got, want)
		}
	}
}

// makeCalls produces n synthetic CallViews with deterministic SIDs so we can
// assert pagination boundaries exactly.
func makeCalls(n int) []CallView {
	out := make([]CallView, n)
	start := time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		out[i] = fakeCall{
			callSid:    testCallSid,
			accountSid: testAccountSid,
			from:       "+49000",
			to:         "+49111",
			direction:  "inbound",
			status:     "in-progress",
			startTime:  start.Add(time.Duration(i) * time.Second),
		}
	}
	return out
}

// TestSerializePage_FirstPageOfMany: 10 items, page=0, pageSize=3 → start=0,
// end=3, calls=3, next_page_uri non-null, previous_page_uri null.
func TestSerializePage_FirstPageOfMany(t *testing.T) {
	t.Parallel()

	pj := SerializePage(makeCalls(10), testPathPrefix, 0, 3)
	if pj.Page != 0 || pj.PageSize != 3 {
		t.Fatalf("page/pageSize: got %d/%d, want 0/3", pj.Page, pj.PageSize)
	}
	if pj.Start != 0 || pj.End != 3 {
		t.Fatalf("start/end: got %d/%d, want 0/3", pj.Start, pj.End)
	}
	if len(pj.Calls) != 3 {
		t.Fatalf("calls len: got %d, want 3", len(pj.Calls))
	}
	if pj.NextPageURI == nil {
		t.Fatal("NextPageURI: got nil, want non-nil")
	}
	if !strings.Contains(*pj.NextPageURI, "Page=1") {
		t.Fatalf("NextPageURI: got %q, want Page=1", *pj.NextPageURI)
	}
	if pj.PreviousPageURI != nil {
		t.Fatalf("PreviousPageURI: got %q, want nil", *pj.PreviousPageURI)
	}
}

// TestSerializePage_LastPage: 10 items, page=3, pageSize=3 → start=9, end=10,
// next=null, prev non-null.
func TestSerializePage_LastPage(t *testing.T) {
	t.Parallel()

	pj := SerializePage(makeCalls(10), testPathPrefix, 3, 3)
	if pj.Start != 9 || pj.End != 10 {
		t.Fatalf("start/end: got %d/%d, want 9/10", pj.Start, pj.End)
	}
	if len(pj.Calls) != 1 {
		t.Fatalf("calls len: got %d, want 1", len(pj.Calls))
	}
	if pj.NextPageURI != nil {
		t.Fatalf("NextPageURI: got %q, want nil", *pj.NextPageURI)
	}
	if pj.PreviousPageURI == nil {
		t.Fatal("PreviousPageURI: got nil, want non-nil")
	}
	if !strings.Contains(*pj.PreviousPageURI, "Page=2") {
		t.Fatalf("PreviousPageURI: got %q, want Page=2", *pj.PreviousPageURI)
	}
}

// TestSerializePage_EmptyList: 0 items, page=0, pageSize=50 → calls=[],
// start=0, end=0, both prev/next null.
func TestSerializePage_EmptyList(t *testing.T) {
	t.Parallel()

	pj := SerializePage(nil, testPathPrefix, 0, 50)
	if pj.Start != 0 || pj.End != 0 {
		t.Fatalf("start/end: got %d/%d, want 0/0", pj.Start, pj.End)
	}
	if pj.Calls == nil {
		t.Fatal("Calls: got nil, want non-nil empty slice (JSON [], not null)")
	}
	if len(pj.Calls) != 0 {
		t.Fatalf("Calls len: got %d, want 0", len(pj.Calls))
	}
	if pj.NextPageURI != nil {
		t.Fatalf("NextPageURI: got %q, want nil", *pj.NextPageURI)
	}
	if pj.PreviousPageURI != nil {
		t.Fatalf("PreviousPageURI: got %q, want nil", *pj.PreviousPageURI)
	}

	// Verify JSON encodes "calls": [] (NOT null).
	b, err := json.Marshal(pj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"calls":[]`) {
		t.Fatalf("expected `calls:[]` in JSON, got: %s", b)
	}
}

// TestSerializePage_PageSizeClamp: pageSize=0 → 50, pageSize=10000 → 1000.
func TestSerializePage_PageSizeClamp(t *testing.T) {
	t.Parallel()

	pj0 := SerializePage(makeCalls(1), testPathPrefix, 0, 0)
	if pj0.PageSize != 50 {
		t.Fatalf("pageSize=0 clamp: got %d, want 50", pj0.PageSize)
	}
	pjBig := SerializePage(makeCalls(1), testPathPrefix, 0, 10000)
	if pjBig.PageSize != 1000 {
		t.Fatalf("pageSize=10000 clamp: got %d, want 1000", pjBig.PageSize)
	}
	pjNeg := SerializePage(makeCalls(1), testPathPrefix, -1, -7)
	if pjNeg.Page != 0 {
		t.Fatalf("page=-1 clamp: got %d, want 0", pjNeg.Page)
	}
	if pjNeg.PageSize != 50 {
		t.Fatalf("pageSize=-7 clamp: got %d, want 50", pjNeg.PageSize)
	}
}

// TestSerializePage_NextPageURI_NullNotOmitted is the contract test for the
// SDKs: on the last page the encoded JSON MUST contain "next_page_uri":null
// — Twilio does NOT omit the field. This guards against an accidental
// `omitempty` tag creeping in.
func TestSerializePage_NextPageURI_NullNotOmitted(t *testing.T) {
	t.Parallel()

	pj := SerializePage(makeCalls(3), testPathPrefix, 0, 50) // single page, last page
	b, err := json.Marshal(pj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"next_page_uri":null`) {
		t.Fatalf("expected `\"next_page_uri\":null` in JSON; got: %s", got)
	}
	if !strings.Contains(got, `"previous_page_uri":null`) {
		t.Fatalf("expected `\"previous_page_uri\":null` in JSON; got: %s", got)
	}
}
