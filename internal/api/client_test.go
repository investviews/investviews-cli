package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewRejectsAnUnusableBaseURL(t *testing.T) {
	for _, base := range []string{"", "   ", "not-a-url", "/public/v1"} {
		if _, err := New(Options{BaseURL: base}); err == nil {
			t.Fatalf("New(%q) should have failed", base)
		}
	}
}

func TestRequestKeepsTheBasePathAndSendsTheToken(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		freeHeaders(w)
		writeJSON(w, 200, `{"results":[]}`)
	})

	if _, err := rec.client.GeoBrowse(context.Background(), GeoParams{Parent: "es", Level: "city"}); err != nil {
		t.Fatalf("GeoBrowse: %v", err)
	}
	got := rec.last()
	if got.Path != "/public/v1/geo" {
		t.Errorf("path = %q, want /public/v1/geo — the base URL's own path must survive", got.Path)
	}
	if got.Query != "level=city&parent=es" {
		t.Errorf("query = %q", got.Query)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer iv_live_ro_testtoken" {
		t.Errorf("Authorization = %q", auth)
	}
}

// /ping answers without a token on purpose: without that, a client cannot tell
// "the service is down" from "my token is wrong".
func TestPingAndOpenAPIAreSentWithoutACredential(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "openapi.yaml") {
			w.WriteHeader(200)
			_, _ = w.Write([]byte("openapi: 3.1.0\n"))
			return
		}
		freeHeaders(w)
		writeJSON(w, 200, `{"status":"ok","version":"v1","docs_url":"https://docs.investviews.ai/"}`)
	})

	ping, err := rec.client.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if ping.Status != "ok" || ping.Version != "v1" {
		t.Errorf("ping = %+v", ping)
	}
	if auth := rec.last().Header.Get("Authorization"); auth != "" {
		t.Errorf("/ping must not carry a credential, got %q", auth)
	}

	body, _, err := rec.client.OpenAPI(context.Background())
	if err != nil {
		t.Fatalf("OpenAPI: %v", err)
	}
	if !strings.HasPrefix(string(body), "openapi:") {
		t.Errorf("OpenAPI body = %q", string(body))
	}
	if auth := rec.last().Header.Get("Authorization"); auth != "" {
		t.Errorf("/openapi.yaml must not carry a credential, got %q", auth)
	}
}

// The server tells you per response whether the call spent quota.
func TestMetaReportsWhetherTheCallSpentQuota(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/public/v1/stats") {
			meteredHeaders(w)
			writeJSON(w, 200, `{"currency":"USD","stats":[]}`)
			return
		}
		freeHeaders(w)
		writeJSON(w, 200, `{"results":[]}`)
	})

	free, err := rec.client.GeoBrowse(context.Background(), GeoParams{})
	if err != nil {
		t.Fatalf("GeoBrowse: %v", err)
	}
	if free.Meta.Metered() {
		t.Error("a /geo call must not read as metered")
	}
	if free.Meta.QuotaLimit != Uncounted {
		t.Errorf("QuotaLimit = %q, want the literal %q", free.Meta.QuotaLimit, Uncounted)
	}
	if free.Meta.QuotaUsed != nil {
		t.Error("X-Quota-Used is absent on a free call; 0 would read as a spent allowance")
	}
	if !strings.Contains(free.Meta.Cost(), "free") {
		t.Errorf("Cost() = %q", free.Meta.Cost())
	}

	metered, err := rec.client.StatsCurrent(context.Background(), StatsParams{GeoID: "es"})
	if err != nil {
		t.Fatalf("StatsCurrent: %v", err)
	}
	if !metered.Meta.Metered() {
		t.Error("a /stats/current call must read as metered")
	}
	if metered.Meta.QuotaGroup != "current" {
		t.Errorf("QuotaGroup = %q", metered.Meta.QuotaGroup)
	}
	if metered.Meta.QuotaUsed == nil || *metered.Meta.QuotaUsed != 138 {
		t.Errorf("QuotaUsed = %v", metered.Meta.QuotaUsed)
	}
	if strings.Contains(metered.Meta.Cost(), "free") {
		t.Errorf("Cost() = %q — a metered call must not look free", metered.Meta.Cost())
	}
}

func TestEndpointGroupsMatchTheContract(t *testing.T) {
	for _, e := range Endpoints() {
		wantMetered := e == EndpointStatsCurrent || e == EndpointStatsHistory
		if e.Metered() != wantMetered {
			t.Errorf("%s Metered() = %v", e, e.Metered())
		}
		if !wantMetered && e.Group() != "metadata" {
			t.Errorf("%s Group() = %q, want metadata", e, e.Group())
		}
	}
	if len(Endpoints()) != 11 {
		t.Errorf("Endpoints() covers %d routes, want the 11 live ones", len(Endpoints()))
	}
}

// ── retry ────────────────────────────────────────────────────────────────────

func TestRetryHonoursRetryAfterOn429(t *testing.T) {
	var calls int
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.Header().Set("Retry-After", "2")
			meteredHeaders(w)
			writeJSON(w, 429, `{"error":"rate_limited","message":"Too many requests: this account is limited to 300 requests per minute on the current endpoint group. Retry in 2 seconds.","docs_url":"https://docs.investviews.ai/errors.html","retry_after":2,"limit":300,"group":"current"}`)
			return
		}
		meteredHeaders(w)
		writeJSON(w, 200, `{"currency":"USD","stats":[]}`)
	})

	res, err := rec.client.StatsCurrent(context.Background(), StatsParams{GeoID: "es"})
	if err != nil {
		t.Fatalf("StatsCurrent: %v", err)
	}
	if rec.hits() != 3 {
		t.Errorf("hits = %d, want 3", rec.hits())
	}
	want := []time.Duration{2 * time.Second, 2 * time.Second}
	if len(rec.sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", rec.sleeps, want)
	}
	for i := range want {
		if rec.sleeps[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v — Retry-After must be honoured verbatim", i, rec.sleeps[i], want[i])
		}
	}
	if res.Meta.Attempts != 3 {
		t.Errorf("Meta.Attempts = %d, want 3", res.Meta.Attempts)
	}
}

func TestRetryBacksOffExponentiallyOn500(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 500, `{"error":"internal_error","message":"The API failed while handling this request.","docs_url":"https://docs.investviews.ai/errors.html","request_id":"8f0c1e2a"}`)
	})

	_, err := rec.client.Usage(context.Background())
	if err == nil {
		t.Fatal("a persistent 500 must end in an error")
	}
	if rec.hits() != 4 {
		t.Errorf("hits = %d, want 4 (one attempt plus %d retries)", rec.hits(), DefaultMaxRetries)
	}
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}
	if len(rec.sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", rec.sleeps, want)
	}
	for i := range want {
		if rec.sleeps[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v", i, rec.sleeps[i], want[i])
		}
	}
	if !errors.Is(err, ErrInternalError) {
		t.Errorf("err = %v, want internal_error", err)
	}
	if !strings.Contains(err.Error(), "8f0c1e2a") {
		t.Errorf("a 500 must quote the request_id: %v", err)
	}
}

func TestBackoffIsBoundedByMaxBackoff(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 503, `{"error":"internal_error","message":"upstream down","docs_url":"https://docs.investviews.ai/errors.html"}`)
	}, func(o *Options) {
		o.MaxRetries = 6
		o.BackoffBase = time.Second
		o.MaxBackoff = 3 * time.Second
	})

	if _, err := rec.client.Usage(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	for i, d := range rec.sleeps {
		if d > 3*time.Second {
			t.Errorf("sleep %d = %v, above MaxBackoff", i, d)
		}
	}
	if rec.sleeps[len(rec.sleeps)-1] != 3*time.Second {
		t.Errorf("the last sleep = %v, want the cap", rec.sleeps[len(rec.sleeps)-1])
	}
}

// ⚠️ Never retry a 4xx that is not 429. A repeated 400 is a 400, and a retried
// invalid_cursor is an infinite loop.
func TestNon429ClientErrorsAreNeverRetried(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"invalid_parameter", 400, `{"error":"invalid_parameter","message":"bad value","docs_url":"d","parameter":"country"}`},
		{"invalid_cursor", 400, `{"error":"invalid_cursor","message":"That cursor was issued for a different query.","docs_url":"d","parameter":"cursor"}`},
		{"invalid_token", 401, `{"error":"invalid_token","message":"Provide a valid API token.","docs_url":"d"}`},
		{"quota_exhausted", 402, `{"error":"quota_exhausted","message":"spent","docs_url":"d"}`},
		{"resolution_not_in_plan", 403, `{"error":"resolution_not_in_plan","message":"too fine","docs_url":"d"}`},
		{"unknown_place", 404, `{"error":"unknown_place","message":"no match","docs_url":"d","geo_id":"R999"}`},
		{"not_covered", 404, `{"error":"not_covered","message":"no data for that country","docs_url":"d"}`},
		{"too_many_hexes", 422, `{"error":"too_many_hexes","message":"too big","docs_url":"d"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				freeHeaders(w)
				writeJSON(w, tc.status, tc.body)
			})
			if _, err := rec.client.Usage(context.Background()); err == nil {
				t.Fatal("expected an error")
			}
			if rec.hits() != 1 {
				t.Errorf("hits = %d, want 1 — a %d must never be retried", rec.hits(), tc.status)
			}
			if len(rec.sleeps) != 0 {
				t.Errorf("sleeps = %v, want none", rec.sleeps)
			}
		})
	}
}

// A retry budget turns a long wait into a fast error. The MCP server sets one:
// an agent's tool call that blocks for two minutes on a 429 reads as hung.
func TestRetryBudgetGivesUpInsteadOfWaiting(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		meteredHeaders(w)
		writeJSON(w, 429, `{"error":"rate_limited","message":"slow down","docs_url":"d","retry_after":30,"limit":300,"group":"current"}`)
	})

	_, err := rec.client.WithRetryBudget(10*time.Second).StatsCurrent(context.Background(), StatsParams{GeoID: "es"})
	var rate *RateLimitError
	if !errors.As(err, &rate) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if rec.hits() != 1 || len(rec.sleeps) != 0 {
		t.Errorf("hits = %d, sleeps = %v — a 30s wait over a 10s budget must not be slept", rec.hits(), rec.sleeps)
	}
}

// The budget is a TOTAL across retries, not a cap per wait: three 4s waits fit
// under 10s one at a time, but not together.
func TestRetryBudgetIsATotalAcrossRetries(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "4")
		meteredHeaders(w)
		writeJSON(w, 429, `{"error":"rate_limited","message":"slow down","docs_url":"d","retry_after":4,"limit":300,"group":"current"}`)
	})

	_, err := rec.client.WithRetryBudget(10*time.Second).StatsCurrent(context.Background(), StatsParams{GeoID: "es"})
	if err == nil {
		t.Fatal("expected an error")
	}
	want := []time.Duration{4 * time.Second, 4 * time.Second}
	if len(rec.sleeps) != len(want) || rec.sleeps[0] != want[0] || rec.sleeps[1] != want[1] {
		t.Errorf("sleeps = %v, want %v — the third 4s wait would cross 10s", rec.sleeps, want)
	}
	if rec.hits() != 3 {
		t.Errorf("hits = %d, want 3", rec.hits())
	}
}

// WithRetryBudget returns a copy. The client the factory built for the CLI
// keeps its two-minute patience.
func TestWithRetryBudgetLeavesTheOriginalAlone(t *testing.T) {
	var calls int
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "30")
			meteredHeaders(w)
			writeJSON(w, 429, `{"error":"rate_limited","message":"slow down","docs_url":"d","retry_after":30,"limit":300,"group":"current"}`)
			return
		}
		meteredHeaders(w)
		writeJSON(w, 200, `{"currency":"USD","stats":[]}`)
	})

	_ = rec.client.WithRetryBudget(time.Second)
	if _, err := rec.client.StatsCurrent(context.Background(), StatsParams{GeoID: "es"}); err != nil {
		t.Fatalf("the original client must still wait out a 30s Retry-After: %v", err)
	}
	if len(rec.sleeps) != 1 || rec.sleeps[0] != 30*time.Second {
		t.Errorf("sleeps = %v, want [30s]", rec.sleeps)
	}
}

// A Retry-After longer than the cap is refused rather than slept through: a
// CLI blocked for an hour on a header looks hung.
func TestAnOverLongRetryAfterIsNotSleptThrough(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		meteredHeaders(w)
		writeJSON(w, 429, `{"error":"rate_limited","message":"slow down","docs_url":"d","retry_after":3600,"limit":300,"group":"current"}`)
	})

	_, err := rec.client.Usage(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if rec.hits() != 1 || len(rec.sleeps) != 0 {
		t.Errorf("hits = %d, sleeps = %v — want no retry", rec.hits(), rec.sleeps)
	}
	var rate *RateLimitError
	if !errors.As(err, &rate) {
		t.Fatalf("err = %T, want *RateLimitError", err)
	}
	if rate.Wait != time.Hour {
		t.Errorf("Wait = %v, want 1h", rate.Wait)
	}
}

func TestRetryAfterAcceptsAnHTTPDate(t *testing.T) {
	at := time.Now().Add(30 * time.Second).UTC()
	d, ok := parseRetryAfter(at.Format(http.TimeFormat), time.Now())
	if !ok {
		t.Fatal("an HTTP-date Retry-After must parse")
	}
	if d < 25*time.Second || d > 31*time.Second {
		t.Errorf("delay = %v, want about 30s", d)
	}
	if _, ok := parseRetryAfter("", time.Now()); ok {
		t.Error("an empty Retry-After is not a delay")
	}
}

func TestTransportFailuresAreRetried(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {})
	rec.server.Close() // nothing is listening any more

	_, err := rec.client.Usage(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	var transport *TransportError
	if !errors.As(err, &transport) {
		t.Fatalf("err = %T, want *TransportError", err)
	}
	if len(rec.sleeps) != DefaultMaxRetries {
		t.Errorf("sleeps = %v, want %d retries", rec.sleeps, DefaultMaxRetries)
	}
	if ExitCode(err) != ExitError {
		t.Errorf("ExitCode = %d, want %d", ExitCode(err), ExitError)
	}
}

// ── context ──────────────────────────────────────────────────────────────────

func TestContextCancelledBeforeTheCall(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		freeHeaders(w)
		writeJSON(w, 200, `{"results":[]}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := rec.client.GeoBrowse(ctx, GeoParams{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rec.hits() != 0 {
		t.Errorf("hits = %d, want 0", rec.hits())
	}
}

func TestContextCancelledDuringARetryWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 500, `{"error":"internal_error","message":"boom","docs_url":"d"}`)
	}, func(o *Options) {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			cancel() // the user pressed Ctrl-C while we were backing off
			return ctx.Err()
		}
	})

	_, err := rec.client.Usage(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rec.hits() != 1 {
		t.Errorf("hits = %d, want 1 — the retry must not be sent after cancellation", rec.hits())
	}
}

func TestContextDeadlineStopsTheRequest(t *testing.T) {
	release := make(chan struct{})
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		writeJSON(w, 200, `{"results":[]}`)
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := rec.client.GeoBrowse(ctx, GeoParams{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// ── body handling ────────────────────────────────────────────────────────────

func TestANonJSONErrorBodyStillProducesATypedError(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(502)
		_, _ = w.Write([]byte("<html><body>Bad Gateway</body></html>"))
	}, func(o *Options) { o.MaxRetries = -1 })

	_, err := rec.client.Usage(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("err = %T, want an *APIError", err)
	}
	if apiErr.Code != CodeInternalError || apiErr.Status != 502 {
		t.Errorf("code = %q status = %d", apiErr.Code, apiErr.Status)
	}
	if !strings.Contains(apiErr.Message, "Bad Gateway") {
		t.Errorf("the raw body should be quoted: %q", apiErr.Message)
	}
}

func TestAnUnreadableSuccessBodyIsReported(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		freeHeaders(w)
		writeJSON(w, 200, `{"results": [`)
	})
	if _, err := rec.client.GeoBrowse(context.Background(), GeoParams{}); err == nil {
		t.Fatal("a truncated body must not decode into an empty result set")
	}
}
