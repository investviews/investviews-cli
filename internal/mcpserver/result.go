package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/investviews/investviews-cli/internal/api"
)

// retryBudget is the most one HTTP request may spend waiting between its
// retries. The whole tool call is bounded separately, by ToolTimeout.
//
// ⚠️ THE CLI WAITS UP TO TWO MINUTES ON A 429; A TOOL CALL MUST NOT. An agent
// that sees no answer for two minutes treats the tool as hung, and cannot tell
// a burst limit from a fault. With this budget a short Retry-After is still
// waited out, and a long one comes back at once as an error whose text says
// "burst limit, not your quota — wait and retry", which the agent can act on.
const retryBudget = 10 * time.Second

// client builds the API client for ONE tool call (rule 2 in the package
// comment) and caps its retry waits. A factory error — ErrNoToken when no
// token is saved — is returned as is, so the handler turns it into a tool
// error on this call.
func (s *server) client() (*api.Client, error) {
	c, err := s.newClient()
	if err != nil {
		return nil, err
	}
	return c.WithRetryBudget(retryBudget), nil
}

// ok is a successful tool result: payload as structuredContent, text as the
// one content block, and a cost line when the call spent quota.
//
// ⚠️ structuredContent IS SET HERE, BY HAND, AND THE HANDLER'S Out IS any.
// Typed Out structs built from api types make the SDK infer an output schema
// and validate every result against it — and api.Ancestors, api.NullableDate
// and the json.RawMessage fields (Filters, Segment, BinApproximations) marshal
// to shapes that schema cannot describe, so a perfectly good answer would come
// back as a protocol error. The payload is marshalled with its own MarshalJSON,
// exactly as the CLI's --json would print it.
//
// Every tool returns BOTH: structuredContent for a client that reads it, and a
// short text for one that only shows content (most do today). The text is a
// summary, not a second copy of the payload.
func ok(payload any, text string, meta api.Meta) (*mcp.CallToolResult, any, error) {
	if line := costText(meta); line != "" {
		text = strings.TrimRight(text, "\n") + "\n" + line
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: payload,
	}, nil, nil
}

// costText is the cost line for a tool result, or "" for a free call.
//
// It is api.Meta.Cost(), not a third copy of the CLI's costLine. A free call
// gets no line at all: the agent was told which tools are free, and a "cost:
// free" under every place search is noise. A response with no quota headers
// DOES get a line ("cost: unknown"), because then the server did not say, and
// silence would read as free.
func costText(meta api.Meta) string {
	if meta.HasQuotaHeaders() && !meta.Metered() {
		return ""
	}
	return meta.Cost()
}

// fail is a failed tool result: isError true, with text the agent can act on.
//
// ⚠️ IT IS A TOOL RESULT, NOT A PROTOCOL ERROR. A JSON-RPC error tells the
// client "the tool is broken" and most clients hide its text from the model.
// A tool result with isError puts the server's message, the docs link and the
// next step in front of the agent, which can then fix its arguments, ask the
// user, or stop.
//
// An API error also carries structuredContent {"error": …} (see toolError):
// the code, the server's wait and the did_you_mean places with their
// ancestry, for a client that reads fields rather than text. Any other error
// (refused locally, transport, no token) has nothing structured to add.
func fail(err error) (*mcp.CallToolResult, any, error) {
	return failNote(err, "")
}

// failNote is fail with one more line: the tool's own next move for an error
// whose generic hint is too vague to act on (price_history's range cap).
func failNote(err error, note string) (*mcp.CallToolResult, any, error) {
	text := errorText(err)
	if note != "" {
		text += "\n" + note
	}
	res := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
	if _, isAPI := api.AsAPIError(err); isAPI {
		res.StructuredContent = map[string]any{"error": errorPayload(err)}
	}
	res.SetError(err)
	return res, nil, nil
}

// toolError is a failed call as fields: the structuredContent of a failed
// tool call, and the error of a failed compare_places row. Message and Hint
// are in the tools' words, like the text.
type toolError struct {
	Code    string `json:"code,omitempty"`
	Status  int    `json:"status,omitempty"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	DocsURL string `json:"docs_url,omitempty"`
	// RetryAfterSeconds is the wait the server asked for on a 429, when it
	// outlasted the retry budget and so was not waited out here.
	RetryAfterSeconds *int `json:"retry_after_seconds,omitempty"`
	// DidYouMean is the places an unknown_place suggests, each with its
	// ancestry: same-named places in one country differ only by it.
	DidYouMean []api.Suggestion `json:"did_you_mean,omitempty"`
}

func errorPayload(err error) *toolError {
	out := &toolError{Message: toolWords.Replace(err.Error())}
	apiErr, isAPI := api.AsAPIError(err)
	if !isAPI {
		return out
	}
	out.Code = string(apiErr.Code)
	out.Status = apiErr.Status
	out.Hint = toolWords.Replace(apiErr.Hint())
	out.DocsURL = apiErr.DocsURL
	if wait, has := retryAfter(err); has {
		secs := int(math.Ceil(wait.Seconds()))
		out.RetryAfterSeconds = &secs
	}
	out.DidYouMean = apiErr.DidYouMean
	return out
}

// retryAfter is the wait a 429 asked for, when the server sent one.
func retryAfter(err error) (time.Duration, bool) {
	var rate *api.RateLimitError
	if errors.As(err, &rate) && rate.HasRetry && rate.Wait > 0 {
		return rate.Wait, true
	}
	return 0, false
}

// errorText is the text of a failed call.
//
//   - An API error is its Error() — message, code and docs_url — plus Hint(),
//     the one-sentence next step. The hint is ADDED, never substituted: the
//     server's message is the authority.
//   - A 429 whose Retry-After outlasted the retry budget says the wait.
//   - An unknown place lists the did_you_mean suggestions, each with the
//     geo_id the agent can spend straight away and its ancestry.
//   - Anything else (a refused request, a transport failure, a missing token)
//     is its Error() as is.
//
// Then every CLI flag and command name is rewritten to the tool's words; see
// toolWords.
func errorText(err error) string {
	var b strings.Builder
	b.WriteString(err.Error())

	if apiErr, isAPI := api.AsAPIError(err); isAPI {
		if hint := apiErr.Hint(); hint != "" {
			b.WriteString("\n")
			b.WriteString(hint)
		}
	}

	if wait, has := retryAfter(err); has {
		fmt.Fprintf(&b, "\nThe server asked for a wait of %ds (Retry-After) before the next call — "+
			"longer than this tool waits on its own. Do not retry before then.", int(math.Ceil(wait.Seconds())))
	}

	if errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintf(&b, "\nThe tool call ran out of its %s limit and was cut off. Retry once; if it "+
			"times out again, the API is slow right now — tell the user and stop.", toolTimeout)
	}

	var miss *api.UnknownPlaceError
	if errors.As(err, &miss) && len(miss.DidYouMean) > 0 {
		b.WriteString("\nDid you mean one of these? Each geo_id can be spent straight away:")
		for _, hit := range miss.DidYouMean {
			fmt.Fprintf(&b, "\n- %s (%s, %s) — geo_id %s", api.Dash(hit.Name), api.Dash(hit.Level),
				api.Dash(hit.Country), api.Dash(hit.GeoID))
			// The chain tells two same-named places in one country apart.
			if hit.Ancestors.Len() > 0 {
				fmt.Fprintf(&b, "\n  in: %s", ancestryText(hit.Ancestors))
			}
		}
	}

	return toolWords.Replace(b.String())
}

// toolWords rewrites the CLI's vocabulary in error text to the MCP tools'.
//
// ⚠️ internal/api'S MESSAGES AND HINTS ARE WRITTEN FOR THE CLI. They name flags
// (--geo-id, --h3, --lat/--lng/--radius-km, --res) and commands (`investviews
// geo search`). An agent calling tools has neither: told to "use --geo-id" it
// either guesses or shells out. So the flags become argument names (geo_id) and
// the commands become tool names (find_place).
//
// Only the flags that ARE tool arguments are listed. `investviews auth login
// --token …` stays as written on purpose: there is no tool for it, and it is a
// command the USER runs in a terminal, so the agent must pass it on verbatim.
// "--token" is listed as itself so that "--to" can never eat its prefix.
var toolWords = strings.NewReplacer(
	// Commands → tools. Longest first: the replacer tries patterns in order.
	"`investviews geo search`", "`find_place`",
	"`investviews geo browse`", "`browse_places`",
	"`investviews stats current`", "`market_stats`",
	"`investviews stats history`", "`price_history`",
	"`investviews coverage`", "the `coverage` tool",
	"`investviews usage`", "the `usage` tool",
	"investviews coverage", "the `coverage` tool",
	"investviews usage", "the `usage` tool",
	"browse from investviews geo", "browse from `browse_places`",

	// Flags → arguments.
	"--token", "--token",
	"--geo-id", "geo_id",
	"--h3", "h3",
	"--radius-km", "radius_km",
	"--lat", "lat",
	"--lng", "lng",
	"--res", "res",
	"--ad-sub-type", "ad_sub_type",
	"--ad-type", "ad_type",
	"--rooms", "rooms",
	"--min-size", "min_size",
	"--max-size", "max_size",
	"--min-price-usd", "min_price_usd",
	"--max-price-usd", "max_price_usd",
	"--currency", "currency",
	"--parent", "parent",
	"--level", "level",
	"--limit", "limit",
	"--page", "page",
	"--country", "country",
	"--from", "from",
	"--to", "to",
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers every tool file uses
// ─────────────────────────────────────────────────────────────────────────────

// readOnly is the annotation set every InvestViews tool carries: it reads, it
// changes nothing, the same call twice is the same call, and it talks to an
// outside service. DestructiveHint is left unset — the spec ignores it when
// ReadOnlyHint is true.
func readOnly(title string) *mcp.ToolAnnotations {
	openWorld := true
	return &mcp.ToolAnnotations{
		Title:          title,
		ReadOnlyHint:   true,
		IdempotentHint: true,
		OpenWorldHint:  &openWorld,
	}
}

func intOr0(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func floatText(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%g", *v)
}

func money(v *float64, currency string) string {
	if v == nil {
		return "—"
	}
	return api.Num(v) + " " + api.Dash(currency)
}

func area(v *float64) string {
	if v == nil {
		return "—"
	}
	return api.Num(v) + " m²"
}

func quoteList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = strconv.Quote(item)
	}
	return strings.Join(quoted, ", ")
}

// indentLines keeps a multi-line error (a hint, did_you_mean suggestions)
// under its row.
func indentLines(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n   ")
}
