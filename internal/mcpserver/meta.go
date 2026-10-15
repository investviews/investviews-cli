package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/investviews/investviews-cli/internal/api"
)

// The three free meta tools. They answer the questions an agent should ask
// BEFORE it spends: which markets exist (coverage), which filter values the
// stats tools accept (filters), and how much quota is left (usage).
//
// ⚠️ ALL THREE ARE UNCOUNTED, AND EACH DESCRIPTION SAYS SO FIRST. An agent that
// is unsure whether a check costs money skips the check and spends the metered
// call blind; "FREE" at the top of the description is what makes it look.

func (s *server) registerMeta(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "coverage",
		Annotations: readOnly("Markets this API serves"),
		Description: `FREE — costs no quota. The markets (countries) this API serves, computed live: for each one, whether it has prices, whether find_place can name places in it, the H3 resolutions held, and when an ad was last parsed.

⚠️ prices and searchable answer different questions: a market can hold prices and still name nothing, so a place there is reachable by h3 cell or by coordinate, not by name.
⚠️ A null last_ad_parsed_at means no source targets that market directly — not that it is empty.

A country code in the answer is a valid geo_id for market_stats: one metered call answers for the whole market. A country with no row is a normal answer: the code is valid and this API holds no market for it.`,
	}, s.coverage)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "filters",
		Annotations: readOnly("Accepted filter values"),
		Description: `FREE — costs no quota. Every value the filter arguments of market_stats, price_history and compare_places accept (ad_type, ad_sub_type, rooms, currency, level, …), and how ranges are read.

Call it when a user's words do not map plainly onto a filter value — guessing a value costs a metered call that answers with an error. ⚠️ Price filters (min_price_usd, max_price_usd) are always USD, whatever currency displays.`,
	}, s.filters)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "usage",
		Annotations: readOnly("Quota used and left"),
		Description: `FREE — costs no quota. What this token's account has spent and has left, per endpoint group, and when each group resets.

Only two groups cost quota: "current" (market_stats, compare_places) and "history" (price_history). They are separate budgets — spending one does not touch the other. An uncounted group has no limit and no remaining (null), which means free, not empty.

Check it before a run of metered calls, and after a quota error.`,
	}, s.usage)
}

// ─────────────────────────────────────────────────────────────────────────────
// coverage
// ─────────────────────────────────────────────────────────────────────────────

type coverageArgs struct {
	Country string `json:"country,omitempty" jsonschema:"restrict to one ISO 3166-1 alpha-2 country code, e.g. es"`
}

func (s *server) coverage(ctx context.Context, _ *mcp.CallToolRequest, args coverageArgs) (*mcp.CallToolResult, any, error) {
	c, err := s.client()
	if err != nil {
		return fail(err)
	}
	resp, err := c.Coverage(ctx, api.CoverageParams{Country: strings.TrimSpace(args.Country)})
	if err != nil {
		return fail(err)
	}
	return ok(resp, coverageText(args.Country, resp), resp.Meta)
}

func coverageText(country string, resp *api.CoverageResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d market(s), measured over a %d-day window.\n", len(resp.Countries), resp.WindowDays)

	if len(resp.Countries) == 0 {
		if c := strings.TrimSpace(country); c != "" {
			fmt.Fprintf(&b, "No market for %q. That is a normal answer, not an error: the code is valid and this API holds no market for it. Call coverage without country to see the markets served.\n", c)
		}
		return b.String()
	}

	for _, row := range resp.Countries {
		fmt.Fprintf(&b, "- %s — prices: %s, searchable: %s, resolutions: %s, last ad parsed: %s\n",
			orDash(row.Country), yesNo(row.PricesAvailable), yesNo(row.Searchable),
			resolutionsText(row.H3Resolutions), lastParsedText(row.LastAdParsedAt))
	}
	b.WriteString("A country code above is a valid geo_id for market_stats. Prices without searchable: reach places by h3 or by coordinate, not by name.\n")
	return b.String()
}

// lastParsedText keeps a null staleness field from reading as "empty".
func lastParsedText(v *string) string {
	if v == nil || strings.TrimSpace(*v) == "" {
		return "none — no source targets this market directly, not empty"
	}
	return *v
}

func resolutionsText(values []int) string {
	if len(values) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, fmt.Sprintf("%d", v))
	}
	return strings.Join(parts, ",")
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// ─────────────────────────────────────────────────────────────────────────────
// filters
// ─────────────────────────────────────────────────────────────────────────────

func (s *server) filters(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	c, err := s.client()
	if err != nil {
		return fail(err)
	}
	resp, err := c.MetaFilters(ctx)
	if err != nil {
		return fail(err)
	}
	return ok(resp, filtersText(resp), resp.Meta)
}

// filtersText lists each filter with its accepted values when it publishes a
// plain list. A filter that publishes something else (price and size publish
// bin geometry) is named and pointed at structuredContent, never dropped: the
// contract says new filters appear here first, so a name this build does not
// know is still worth showing.
func filtersText(resp *api.FiltersResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Filter vocabulary %s.\n", orDash(resp.Version))
	if note := strings.TrimSpace(resp.RangeSemantics); note != "" {
		fmt.Fprintf(&b, "ranges: %s\n", note)
	}

	// Map order is random; a list that moved between calls is unquotable.
	names := make([]string, 0, len(resp.Filters))
	for name := range resp.Filters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if values, listed := resp.Values(name); listed {
			fmt.Fprintf(&b, "- %s: %s\n", name, strings.Join(values, ", "))
		} else {
			fmt.Fprintf(&b, "- %s: not a value list — see structuredContent.filters.%s\n", name, name)
		}
	}
	b.WriteString("Price filters (min_price_usd, max_price_usd) are always USD, whatever currency displays.\n")
	if url := strings.TrimSpace(resp.DocsURL); url != "" {
		fmt.Fprintf(&b, "docs: %s\n", url)
	}
	return b.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// usage
// ─────────────────────────────────────────────────────────────────────────────

// groupOrder puts the two groups that cost money first, then the free ones,
// then any group this build does not know, by name. Same order as the CLI's
// usage table.
var groupOrder = map[string]int{"current": 0, "history": 1, "reports": 2, "metadata": 3}

// groupTools names the tools that spend each metered group, so the agent can
// map "current: 12 left" onto the calls it is about to make.
var groupTools = map[string]string{
	"current": "market_stats, compare_places",
	"history": "price_history",
}

func (s *server) usage(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	c, err := s.client()
	if err != nil {
		return fail(err)
	}
	resp, err := c.Usage(ctx)
	if err != nil {
		return fail(err)
	}
	return ok(resp, usageText(resp), resp.Meta)
}

func usageText(resp *api.UsageResponse) string {
	var b strings.Builder
	access := "read/write"
	if resp.Token.ReadOnly {
		access = "read-only"
	}
	fmt.Fprintf(&b, "Token %q (%s) — %s, %s.\n", resp.Token.Name, orDash(resp.Token.MaskedToken),
		orDash(resp.Token.Status), access)

	names := make([]string, 0, len(resp.Groups))
	for name := range resp.Groups {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		oi, iKnown := groupOrder[names[i]]
		oj, jKnown := groupOrder[names[j]]
		switch {
		case iKnown && jKnown:
			return oi < oj
		case iKnown != jKnown:
			return iKnown
		default:
			return names[i] < names[j]
		}
	})

	var degraded bool
	for _, name := range names {
		g := resp.Groups[name]
		degraded = degraded || g.Degraded
		if !g.Metered {
			// ⚠️ NEVER "0 remaining": on an uncounted group the counters are
			// null, and a zero would tell the agent the user is out.
			fmt.Fprintf(&b, "- %s: free — uncounted, no limit\n", name)
			continue
		}
		line := fmt.Sprintf("- %s: %s remaining of %s (used %s), resets %s",
			name, countText(g.Remaining), countText(g.Limit), countText(g.Used), strText(g.ResetsAt))
		if tools, known := groupTools[name]; known {
			line += " — spent by " + tools
		}
		b.WriteString(line + "\n")
	}

	b.WriteString(`"current" and "history" are separate budgets: spending one does not touch the other.` + "\n")
	if degraded {
		b.WriteString("⚠️ degraded: the metering store was unreachable, so these figures are the committed balance alone and may lag. Requests are still served.\n")
	}
	return b.String()
}

// countText renders an optional counter. nil is "unknown", never 0.
func countText(v *int64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%d", *v)
}
