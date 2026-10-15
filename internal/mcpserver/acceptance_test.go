package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/investviews/investviews-cli/internal/api"
)

// ── acceptance: the whole tool set, every failure kind ───────────────────────
//
// The per-tool tests prove each tool's own behaviour. These two prove the
// promises the server makes for ALL tools at once, so a tool added later cannot
// quietly break them: every tool says up front whether it costs quota, and
// every failure an agent can meet reaches it as a tool result it can read.

// toolCost is the cost word each tool's description must open with — the
// table in the plan's Solution Overview.
var toolCost = map[string]string{
	"find_place":     "FREE",
	"browse_places":  "FREE",
	"place_at_point": "FREE",
	"coverage":       "FREE",
	"filters":        "FREE",
	"usage":          "FREE",
	"market_stats":   "METERED",
	"price_history":  "METERED",
	"compare_places": "METERED",
}

// validArgs is the smallest argument set each tool accepts, so the failure a
// test injects is the only thing wrong with the call.
var validArgs = map[string]map[string]any{
	"find_place":     {"q": "valencia"},
	"browse_places":  {},
	"place_at_point": {"lat": 39.47, "lng": -0.37},
	"coverage":       {},
	"filters":        {},
	"usage":          {},
	"market_stats":   {"geo_id": "R1"},
	"price_history":  {"geo_id": "R1"},
	"compare_places": {"geo_ids": []any{"R1", "R2"}},
}

func TestEveryToolIsAnnouncedFreeOrMeteredAndReadOnly(t *testing.T) {
	session, fake := connectFake(t, nil)

	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(list.Tools) != len(toolCost) {
		t.Errorf("tools/list returned %d tools, want %d", len(list.Tools), len(toolCost))
	}
	seen := map[string]bool{}
	for _, tool := range list.Tools {
		seen[tool.Name] = true
		cost, known := toolCost[tool.Name]
		if !known {
			t.Errorf("tool %q is not in the cost table — add it, with its cost", tool.Name)
			continue
		}
		if !strings.HasPrefix(tool.Description, cost+" — ") {
			t.Errorf("%s description does not open with %q: %q", tool.Name, cost+" — ", tool.Description)
		}
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Errorf("%s annotations = %+v, want read-only, idempotent, open-world", tool.Name, a)
		}
		if a != nil && strings.TrimSpace(a.Title) == "" {
			t.Errorf("%s has no title", tool.Name)
		}
	}
	for name := range toolCost {
		if !seen[name] {
			t.Errorf("tools/list is missing %q", name)
		}
	}
	if fake.hits() != 0 {
		t.Errorf("tools/list sent %d API requests, want 0", fake.hits())
	}
}

// errorReply answers every request with one API error.
func errorReply(status int, body string, headers map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		writeBody(w, status, body)
	}
}

// ⚠️ EVERY FAILURE IS A TOOL RESULT WITH isError, NEVER A PROTOCOL ERROR. A
// protocol error is shown to the user as "the tool failed"; the agent cannot
// read it and cannot act on it. callTool fails the test on a protocol error,
// so each case below proves both halves: a result came back, and it is marked
// as an error with text that says what to do.
func TestEveryToolReturnsEveryFailureAsAToolResult(t *testing.T) {
	noToken := errors.New(`no API token configured; run "investviews auth login --token …" or set INVESTVIEWS_TOKEN`)

	cases := []struct {
		name    string
		handler http.HandlerFunc // nil: the factory fails before any request
		args    func(tool string) any
		want    string
		maxHits int
	}{
		{
			name: "no token",
			args: func(tool string) any { return validArgs[tool] },
			want: "investviews auth login --token",
		},
		{
			name: "bad token",
			handler: errorReply(http.StatusUnauthorized, `{"error":"invalid_token",`+
				`"message":"The API token is invalid or has been revoked.",`+
				`"docs_url":"https://docs.investviews.ai/errors.html#invalid_token"}`, nil),
			args:    func(tool string) any { return validArgs[tool] },
			want:    "[invalid_token]",
			maxHits: 1,
		},
		{
			name: "quota",
			handler: errorReply(http.StatusPaymentRequired, `{"error":"quota_exhausted",`+
				`"message":"Monthly quota for group current is exhausted.",`+
				`"docs_url":"https://docs.investviews.ai/errors.html#quota_exhausted",`+
				`"group":"current","limit":100,"used":100,"reset_at":"2026-11-01T00:00:00Z"}`, nil),
			args:    func(tool string) any { return validArgs[tool] },
			want:    "[quota_exhausted]",
			maxHits: 1,
		},
		{
			name: "rate limit",
			handler: errorReply(http.StatusTooManyRequests, `{"error":"rate_limited",`+
				`"message":"Too many requests.","docs_url":"d","retry_after":60,"limit":300,"group":"current"}`,
				map[string]string{"Retry-After": "60"}),
			args:    func(tool string) any { return validArgs[tool] },
			want:    "[rate_limited]",
			maxHits: 1,
		},
		{
			// Arguments that are not an object fail the input schema of
			// every tool, in the SDK, before the handler runs.
			name:    "schema validation",
			handler: errorReply(http.StatusInternalServerError, `{}`, nil),
			args:    func(string) any { return []any{1} },
			want:    "arguments",
		},
	}

	for _, tc := range cases {
		for _, tool := range wantTools {
			t.Run(tc.name+"/"+tool, func(t *testing.T) {
				f := newFakeAPI(t, tc.handler)
				factory := f.factory(t)
				if tc.handler == nil {
					factory = func() (*api.Client, error) { return nil, noToken }
				}
				session := connect(t, factory)

				res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
					Name:      tool,
					Arguments: tc.args(tool),
				})
				if err != nil {
					t.Fatalf("returned a protocol error, want a tool result with isError: %v", err)
				}
				if !res.IsError {
					t.Fatalf("not marked isError: %s", textOf(t, res))
				}
				mustContain(t, textOf(t, res), tc.want)
				// One request at most: a refused call is never retried
				// into a second spend, and a compare stops at the first.
				if f.hits() > tc.maxHits {
					t.Errorf("sent %d requests, want at most %d", f.hits(), tc.maxHits)
				}
			})
		}
	}
}
