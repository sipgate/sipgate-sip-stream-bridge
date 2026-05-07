package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// TestError_WriteJSON_ShapeMatch asserts that WriteJSON emits exactly the
// Twilio-shape JSON body, the documented Content-Type, and the configured HTTP
// status. The decode-and-compare avoids any field-ordering brittleness while
// still verifying byte-level identity for the value set.
func TestError_WriteJSON_ShapeMatch(t *testing.T) {
	t.Parallel()

	e := ErrAuthRequired()
	rec := httptest.NewRecorder()
	e.WriteJSON(rec)

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type: got %q, want %q", got, "application/json")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	var decoded Error
	if err := json.NewDecoder(rec.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if decoded.Code != 20003 {
		t.Fatalf("code: got %d, want 20003", decoded.Code)
	}
	if decoded.Status != 401 {
		t.Fatalf("status field: got %d, want 401", decoded.Status)
	}
	if decoded.Message == "" {
		t.Fatal("message: empty")
	}
	if decoded.MoreInfo != "https://www.twilio.com/docs/errors/20003" {
		t.Fatalf("more_info: got %q, want %q",
			decoded.MoreInfo, "https://www.twilio.com/docs/errors/20003")
	}
}

// TestErrPayloadTooLarge pins the (Code, Status, Message, MoreInfo) tuple of
// the anti-DoS 413 constructor. Used by api/security.go MaxBytesReader
// middleware when a REST request body exceeds the 64KB cap.
//
// Twilio code 21617 is the closest semantic match in Twilio's published error
// vocabulary for body-size violations — no dedicated 413 code exists. The
// constructor's godoc documents this rationale.
func TestErrPayloadTooLarge(t *testing.T) {
	t.Parallel()

	e := ErrPayloadTooLarge()

	if e.Code != 21617 {
		t.Fatalf("code: got %d, want 21617", e.Code)
	}
	if e.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want %d (413)", e.Status, http.StatusRequestEntityTooLarge)
	}
	if !regexp.MustCompile(`64KB`).MatchString(e.Message) {
		t.Fatalf("message: got %q, want it to contain \"64KB\"", e.Message)
	}
	if e.MoreInfo != "https://www.twilio.com/docs/errors/21617" {
		t.Fatalf("more_info: got %q, want %q",
			e.MoreInfo, "https://www.twilio.com/docs/errors/21617")
	}
}

// TestError_PrebuiltConstructors_AllCodesUnique iterates every prebuilt
// constructor and verifies (Code, HTTP-Status) parity with Twilio's published
// error codes. Also asserts each Code is unique — guards against future
// copy-paste regressions.
func TestError_PrebuiltConstructors_AllCodesUnique(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		err        *Error
		wantCode   int
		wantStatus int
	}{
		{"ErrAuthRequired", ErrAuthRequired(), 20003, http.StatusUnauthorized},
		{"ErrNotFound", ErrNotFound("/Calls/CAxxxx.json"), 20404, http.StatusNotFound},
		{"ErrInvalidParams", ErrInvalidParams("Url"), 21218, http.StatusBadRequest},
		{"ErrCallNotInProgress", ErrCallNotInProgress(), 21220, http.StatusBadRequest},
		{"ErrTwimlParseFailure", ErrTwimlParseFailure(), 12100, http.StatusBadRequest},
		{"ErrTooManyRequests", ErrTooManyRequests(), 20429, http.StatusTooManyRequests},
		{"ErrPayloadTooLarge", ErrPayloadTooLarge(), 21617, http.StatusRequestEntityTooLarge},
	}

	seen := map[int]string{}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.err.Code != tc.wantCode {
				t.Fatalf("%s: code got %d, want %d", tc.name, tc.err.Code, tc.wantCode)
			}
			if tc.err.Status != tc.wantStatus {
				t.Fatalf("%s: status got %d, want %d", tc.name, tc.err.Status, tc.wantStatus)
			}
			if tc.err.Message == "" {
				t.Fatalf("%s: empty message", tc.name)
			}
		})
	}

	// Uniqueness check (sequential — race-free even though sub-tests are parallel
	// because we iterate the slice, not t.Parallel() goroutines).
	for _, tc := range cases {
		if prev, ok := seen[tc.err.Code]; ok {
			t.Fatalf("duplicate Code %d: %s and %s", tc.err.Code, prev, tc.name)
		}
		seen[tc.err.Code] = tc.name
	}
}

// TestError_MoreInfoFormat asserts every prebuilt constructor produces a
// MoreInfo URL matching Twilio's canonical pattern. Guards against typos in
// newError when codes change.
func TestError_MoreInfoFormat(t *testing.T) {
	t.Parallel()

	moreInfoRE := regexp.MustCompile(`^https://www\.twilio\.com/docs/errors/\d+$`)
	for _, e := range []*Error{
		ErrAuthRequired(),
		ErrNotFound("X"),
		ErrInvalidParams("Y"),
		ErrCallNotInProgress(),
		ErrTwimlParseFailure(),
		ErrTooManyRequests(),
		ErrPayloadTooLarge(),
	} {
		if !moreInfoRE.MatchString(e.MoreInfo) {
			t.Fatalf("MoreInfo %q does not match %s", e.MoreInfo, moreInfoRE)
		}
	}
}
