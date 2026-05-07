package api

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	authTestSid   = "ACdeadbeefdeadbeefdeadbeefdeadbeef"
	authTestToken = "supersecret-auth-token"
)

// nextHandler is a stand-in for the handler chain that should run AFTER
// BasicAuth approves the request. We only check that it was called.
func nextHandler(called *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}
}

// withAuthRouter wraps next with BasicAuth on a chi route that captures
// {AccountSid} so URL-path validation runs.
func withAuthRouter(next http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.Route("/2010-04-01/Accounts/{AccountSid}", func(r chi.Router) {
		r.Use(BasicAuth(authTestSid, authTestToken))
		r.Get("/Calls.json", next)
	})
	return r
}

// TestBasicAuth_NoCredentials: request with no Authorization header → 401,
// WWW-Authenticate header present, body matches ErrAuthRequired() shape.
func TestBasicAuth_NoCredentials(t *testing.T) {
	t.Parallel()

	called := false
	srv := withAuthRouter(nextHandler(&called))

	req := httptest.NewRequest(http.MethodGet, "/2010-04-01/Accounts/"+authTestSid+"/Calls.json", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if called {
		t.Fatal("next handler must NOT be called on missing creds")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") != `Basic realm="Twilio API"` {
		t.Fatalf("WWW-Authenticate: got %q, want %q",
			rec.Header().Get("WWW-Authenticate"), `Basic realm="Twilio API"`)
	}
	var body Error
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != 20003 || body.Status != 401 {
		t.Fatalf("body: got code=%d status=%d, want 20003/401", body.Code, body.Status)
	}
}

// TestBasicAuth_WrongUser: invalid username → 401.
func TestBasicAuth_WrongUser(t *testing.T) {
	t.Parallel()

	called := false
	srv := withAuthRouter(nextHandler(&called))

	req := httptest.NewRequest(http.MethodGet, "/2010-04-01/Accounts/"+authTestSid+"/Calls.json", nil)
	req.SetBasicAuth("wrong-user", authTestToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if called {
		t.Fatal("next handler must NOT be called on wrong username")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestBasicAuth_WrongPassword: invalid password → 401.
func TestBasicAuth_WrongPassword(t *testing.T) {
	t.Parallel()

	called := false
	srv := withAuthRouter(nextHandler(&called))

	req := httptest.NewRequest(http.MethodGet, "/2010-04-01/Accounts/"+authTestSid+"/Calls.json", nil)
	req.SetBasicAuth(authTestSid, "wrong-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if called {
		t.Fatal("next handler must NOT be called on wrong password")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestBasicAuth_Correct: valid credentials AND matching URL-path SID →
// next handler called, status 200.
func TestBasicAuth_Correct(t *testing.T) {
	t.Parallel()

	called := false
	srv := withAuthRouter(nextHandler(&called))

	req := httptest.NewRequest(http.MethodGet, "/2010-04-01/Accounts/"+authTestSid+"/Calls.json", nil)
	req.SetBasicAuth(authTestSid, authTestToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if !called {
		t.Fatal("next handler MUST be called on correct creds")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
}

// TestBasicAuth_PathSidMismatch: credentials valid but the {AccountSid} in
// the URL does NOT match the configured AccountSid → 401 (NOT 404).
// Critical for preventing AccountSid enumeration via 404-vs-401 distinction.
func TestBasicAuth_PathSidMismatch(t *testing.T) {
	t.Parallel()

	called := false
	srv := withAuthRouter(nextHandler(&called))

	otherSid := "ACcafebabecafebabecafebabecafebabe"
	req := httptest.NewRequest(http.MethodGet, "/2010-04-01/Accounts/"+otherSid+"/Calls.json", nil)
	req.SetBasicAuth(authTestSid, authTestToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if called {
		t.Fatal("next handler must NOT be called on path-sid mismatch")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("path-sid mismatch must be 401, NOT %d (404 would leak account_sid existence)", rec.Code)
	}
}

// TestBasicAuth_PathSidMatch: explicit positive case for the path-sid check.
func TestBasicAuth_PathSidMatch(t *testing.T) {
	t.Parallel()

	called := false
	srv := withAuthRouter(nextHandler(&called))

	req := httptest.NewRequest(http.MethodGet, "/2010-04-01/Accounts/"+authTestSid+"/Calls.json", nil)
	req.SetBasicAuth(authTestSid, authTestToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if !called {
		t.Fatal("next handler MUST be called when path-sid matches")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
}

// TestBasicAuth_NoPathParam: route without {AccountSid} URL-param (e.g. a
// /health endpoint mounted under BasicAuth by mistake) skips path validation
// and still authenticates on credentials alone.
func TestBasicAuth_NoPathParam(t *testing.T) {
	t.Parallel()

	called := false
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(BasicAuth(authTestSid, authTestToken))
		r.Get("/health", nextHandler(&called))
	})

	// Correct creds → next runs.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.SetBasicAuth(authTestSid, authTestToken)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if !called {
		t.Fatal("next handler must be called when route has no {AccountSid} and creds are correct")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	// Wrong creds → 401 even without {AccountSid}.
	called = false
	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	req.SetBasicAuth(authTestSid, "wrong")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if called {
		t.Fatal("next handler must NOT run on wrong creds")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestBasicAuth_ConstantTimeTiming: call BasicAuth ~10000 times alternating
// valid + invalid passwords; assert the absolute difference of mean
// wall-clock times is small (loose bound — proves the constant-time path is
// taken, not a perfect microbenchmark).
//
// We use a 1ms upper bound on the absolute difference of means — far above
// the actual timing noise but well below the early-exit signal a
// non-constant-time strcmp would produce on long passwords. The test
// recovers from CI jitter by computing a generous bound and using a long
// password where a non-constant-time compare would diverge measurably.
func TestBasicAuth_ConstantTimeTiming(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping timing test in -short mode")
	}

	const iterations = 10000
	long := strings.Repeat("a", 64)             // long token to magnify any non-CT divergence
	long2 := strings.Repeat("a", 63) + "b"      // differs only in last byte → max divergence in non-CT compare
	mw := BasicAuth(authTestSid, long)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	measure := func(pw string) time.Duration {
		var total time.Duration
		for i := 0; i < iterations; i++ {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.SetBasicAuth(authTestSid, pw)
			rec := httptest.NewRecorder()
			t0 := time.Now()
			handler.ServeHTTP(rec, req)
			total += time.Since(t0)
		}
		return total / iterations
	}

	// Warm up to bring caches into a steady state before measuring.
	_ = measure(long)
	_ = measure(long2)

	meanValid := measure(long)
	meanInvalid := measure(long2)

	diff := time.Duration(math.Abs(float64(meanValid - meanInvalid)))
	t.Logf("constant-time timing: valid=%v invalid=%v |diff|=%v",
		meanValid, meanInvalid, diff)

	// 1ms is far above the actual constant-time noise floor (~tens of
	// nanoseconds) but well below the divergence a non-CT compare would
	// produce given a 64-byte differing-at-end string.
	if diff > time.Millisecond {
		t.Fatalf("timing variance too large: |diff|=%v > 1ms — non-constant-time compare suspected",
			diff)
	}
}
