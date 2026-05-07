package api

import (
	"fmt"
	"time"
)

// CallView is the read-only contract this package needs from a bridge call to
// produce its JSON wire form. We define it locally (rather than importing
// bridge.BridgeCall) so internal/api stays decoupled from internal/bridge —
// thanks to Go's structural interface satisfaction, *bridge.CallSession
// satisfies CallView automatically at the router wire-up site.
//
// All methods MUST be safe to call concurrently from the HTTP handler
// goroutine — implementations are expected to copy state under their own lock
// before returning primitive values.
type CallView interface {
	CallSid() string
	AccountSid() string
	From() string
	To() string
	Direction() string
	Status() string
	StartTime() time.Time
	EndTime() time.Time
	Duration() int
	AnsweredBy() string
	ParentCallSid() string
}

// RFC2822 formats t as Twilio's RFC 2822 timestamp:
//
//	"Tue, 27 Apr 2026 10:00:00 +0000"
//
// Twilio always emits UTC; we Format with t.UTC() so non-UTC inputs are
// normalized and the offset is always "+0000" (not "+0200" or similar).
// time.RFC1123Z uses the layout "Mon, 02 Jan 2006 15:04:05 -0700" — this is
// the wire format Twilio's serializer produces, so we match byte-for-byte.
//
// Returns "" for the zero time so callers can decide between emitting `null`
// (preferred — pointer-typed JSON field) or omitting.
func RFC2822(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC1123Z)
}

// CallJSON is the Twilio-shaped Call resource. Pointer-typed fields are the
// nullables: encoding/json emits explicit `null` (NOT "" or 0) when the
// pointer is nil, matching Twilio's wire contract that twilio-go and
// twilio-python depend on.
type CallJSON struct {
	Sid             string            `json:"sid"`
	AccountSid      string            `json:"account_sid"`
	From            string            `json:"from"`
	To              string            `json:"to"`
	Status          string            `json:"status"`
	StartTime       *string           `json:"start_time"`
	EndTime         *string           `json:"end_time"`
	Duration        *int              `json:"duration"`
	Direction       string            `json:"direction"`
	AnsweredBy      *string           `json:"answered_by"`
	ParentCallSid   *string           `json:"parent_call_sid"`
	APIVersion      string            `json:"api_version"`
	URI             string            `json:"uri"`
	SubresourceURIs map[string]string `json:"subresource_uris"`
}

// SerializeCall converts a CallView into the Twilio-shape JSON struct.
//
// pathPrefix is the AccountSid-bearing route prefix used to build URIs:
//
//	"/2010-04-01/Accounts/ACxxxx"
//
// Subresource URIs are included for parity with Twilio (notifications,
// recordings, events, siprec) even though we do not implement those endpoints
// yet — the SDKs surface them via typed accessors and breaking that contract
// would surface as user-facing errors. Plans 16/17 wire the actual handlers.
func SerializeCall(c CallView, pathPrefix string) *CallJSON {
	cj := &CallJSON{
		Sid:        c.CallSid(),
		AccountSid: c.AccountSid(),
		From:       c.From(),
		To:         c.To(),
		Status:     c.Status(),
		Direction:  c.Direction(),
		APIVersion: "2010-04-01",
		URI:        fmt.Sprintf("%s/Calls/%s.json", pathPrefix, c.CallSid()),
		SubresourceURIs: map[string]string{
			"notifications": fmt.Sprintf("%s/Calls/%s/Notifications.json", pathPrefix, c.CallSid()),
			"recordings":    fmt.Sprintf("%s/Calls/%s/Recordings.json", pathPrefix, c.CallSid()),
			"events":        fmt.Sprintf("%s/Calls/%s/Events.json", pathPrefix, c.CallSid()),
			"siprec":        fmt.Sprintf("%s/Calls/%s/Siprec.json", pathPrefix, c.CallSid()),
		},
	}
	if !c.StartTime().IsZero() {
		s := RFC2822(c.StartTime())
		cj.StartTime = &s
	}
	if !c.EndTime().IsZero() {
		s := RFC2822(c.EndTime())
		cj.EndTime = &s
		d := c.Duration()
		cj.Duration = &d
	}
	if a := c.AnsweredBy(); a != "" {
		cj.AnsweredBy = &a
	}
	if p := c.ParentCallSid(); p != "" {
		cj.ParentCallSid = &p
	}
	return cj
}

// PageJSON is the Twilio page envelope for a list of Call resources.
//
// NextPageURI and PreviousPageURI are pointer-typed so encoding/json emits
// explicit `null` on the boundaries — Twilio's contract is that these fields
// are PRESENT-but-null on first/last pages, NOT omitted. The SDKs check for
// the field's existence to decide whether the response shape is valid.
type PageJSON struct {
	Page            int         `json:"page"`
	PageSize        int         `json:"page_size"`
	Start           int         `json:"start"`
	End             int         `json:"end"`
	URI             string      `json:"uri"`
	NextPageURI     *string     `json:"next_page_uri"`
	PreviousPageURI *string     `json:"previous_page_uri"`
	FirstPageURI    string      `json:"first_page_uri"`
	Calls           []*CallJSON `json:"calls"`
}

// SerializePage paginates a slice of CallView and produces a Twilio-shape
// envelope.
//
// Conventions:
//   - page is 0-indexed (Twilio convention).
//   - pageSize is hard-clamped to [1, 1000]; values <1 default to 50, values
//     >1000 are clipped to 1000 (memory-growth guard).
//   - pathPrefix is the AccountSid-bearing route prefix (used to build URIs).
//   - On out-of-range pages the calls slice is empty but start/end still
//     reflect the clamped indices.
//
// The empty calls slice is allocated as a non-nil zero-length slice so JSON
// encodes `"calls": []` (NOT `"calls": null`) — Twilio's contract.
func SerializePage(items []CallView, pathPrefix string, page, pageSize int) *PageJSON {
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 1000 {
		pageSize = 1000
	}
	if page < 0 {
		page = 0
	}

	total := len(items)
	start := page * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}

	slice := items[start:end]
	callsJSON := make([]*CallJSON, len(slice))
	for i, c := range slice {
		callsJSON[i] = SerializeCall(c, pathPrefix)
	}

	currentURI := fmt.Sprintf("%s/Calls.json?Page=%d&PageSize=%d", pathPrefix, page, pageSize)
	firstURI := fmt.Sprintf("%s/Calls.json?Page=0&PageSize=%d", pathPrefix, pageSize)

	var nextURI, prevURI *string
	if end < total {
		s := fmt.Sprintf("%s/Calls.json?Page=%d&PageSize=%d", pathPrefix, page+1, pageSize)
		nextURI = &s
	}
	if page > 0 {
		s := fmt.Sprintf("%s/Calls.json?Page=%d&PageSize=%d", pathPrefix, page-1, pageSize)
		prevURI = &s
	}

	return &PageJSON{
		Page:            page,
		PageSize:        pageSize,
		Start:           start,
		End:             end,
		URI:             currentURI,
		NextPageURI:     nextURI,
		PreviousPageURI: prevURI,
		FirstPageURI:    firstURI,
		Calls:           callsJSON,
	}
}
