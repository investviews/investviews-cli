package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/investviews/investviews-cli/internal/api"
)

// compare_places is the one workflow tool: market_stats for 2–5 places in one
// call, with the same filters, folded into one row per place. It adds nothing
// the API does not already answer — it saves the agent five round trips and
// gives it a comparison it cannot word differently from market_stats, because
// the rows are built from the same coreFigures.
//
// ⚠️ IT SPENDS ONE METERED CALL PER PLACE. The description says so first, so
// the agent can ask before spending five.
//
// ⚠️ A FAILED PLACE IS A ROW, NOT A FAILED TOOL CALL. An unknown geo_id or an
// empty answer for place 2 must not throw away the figures already paid for
// places 1 and 3. The loop stops early only when the NEXT call cannot succeed
// either — a token, subscription, quota or burst-limit refusal — and even then
// the rows already fetched are returned.

const (
	minComparePlaces = 2
	maxComparePlaces = 5
)

func (s *server) registerCompare(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "compare_places",
		Annotations: readOnly("Compare 2–5 places side by side"),
		Description: fmt.Sprintf(`METERED — ONE call against the "current" quota group PER PLACE: comparing 5 places spends 5. Ask the user before comparing more places than the question needs.

Runs market_stats for each geo_id in order (%d to %d places, all with the same filters) and returns one row per place: median asking price per m², median asking price, median size, the period, and the availability verdict when the place is empty.

geo_ids come from find_place / browse_places (or ISO country codes). Resolve them with the free tools first: an unknown geo_id still costs a request and comes back as an error row.

A place that fails or is empty becomes a row saying why; the other places are still answered. The tool stops early only on a token, subscription, quota or rate-limit refusal, a refused filter value (every place shares it) or the call's time limit, and then returns the rows it already has and how many calls it spent. ⚠️ Without currency each place answers in its own currency — pass currency to compare like with like. Price filters are USD whatever currency displays.`,
			minComparePlaces, maxComparePlaces),
	}, s.comparePlaces)
}

// comparePlacesArgs is a list of places plus market_stats' filters. There is
// no territory selector beyond geo_ids: h3 cells and circles are not places a
// user compares by name.
type comparePlacesArgs struct {
	GeoIDs []string `json:"geo_ids" jsonschema:"2 to 5 geo_ids from find_place or browse_places (or ISO country codes), compared in this order; each costs one metered call"`
	statsFilterArgs
}

// compareAnswer is the structuredContent of compare_places.
//
// ⚠️ IT IS AN OBJECT WRAPPING THE ROWS, NOT A BARE LIST. MCP defines
// structuredContent as a JSON object, and a client that validates it would
// reject an array. The rows are under "rows", in the order asked.
type compareAnswer struct {
	Rows []compareRow `json:"rows"`
	// MeteredCalls is how many answers the server reported as metered;
	// Requests is how many places were sent at all, whatever came back. A
	// place refused locally (a bad filter value caught before sending) is
	// not a request.
	MeteredCalls int `json:"metered_calls"`
	Requests     int `json:"requests"`
	// Stopped is set when the loop ended early: the code that stopped it,
	// and the geo_ids never asked about.
	Stopped    string   `json:"stopped,omitempty"`
	NotAsked   []string `json:"not_asked,omitempty"`
	Currencies []string `json:"currencies,omitempty"`
	// Filters is the filters as the server applied them, from the first
	// place that answered. Every place is sent the same filters, so they are
	// stated once, not per row.
	Filters *api.Filters `json:"filters,omitempty"`
}

// compareRow is one place. Exactly one of three shapes:
//   - figures: the place answered one row — the three core figures are
//     flattened here, nil where the aggregate held nothing (never 0);
//   - empty: no row, and Availability says why;
//   - error: the call failed, and Error says how.
//
// A place that answered several rows (not expected for a geo_id) keeps them
// all in Stats and no flattened figure: picking one would hide the others.
type compareRow struct {
	GeoID   string `json:"geo_id"`
	Name    string `json:"name,omitempty"`
	Level   string `json:"level,omitempty"`
	Country string `json:"country,omitempty"`

	Period *string `json:"period,omitempty"`
	// Granularity and the window are what the period MEANS: two places with
	// the same period date can still be aggregated over different windows
	// (a month against a rolling three months), and then they are not
	// comparable as printed.
	Granularity string  `json:"granularity,omitempty"`
	WindowStart *string `json:"window_start,omitempty"`
	WindowEnd   *string `json:"window_end,omitempty"`
	AsOf        *string `json:"as_of,omitempty"`
	Currency    string  `json:"currency,omitempty"`

	MedianPricePerSqm *float64   `json:"median_price_per_sqm,omitempty"`
	MedianPrice       *float64   `json:"median_price,omitempty"`
	MedianArea        *float64   `json:"median_area,omitempty"`
	P05PricePerSqm    *float64   `json:"p05_price_per_sqm,omitempty"`
	P95PricePerSqm    *float64   `json:"p95_price_per_sqm,omitempty"`
	Estimated         bool       `json:"estimated,omitempty"`
	LowConfidence     *bool      `json:"low_confidence,omitempty"`
	Stats             []api.Stat `json:"stats,omitempty"`

	Availability *api.Availability `json:"availability,omitempty"`
	// BinApproximations is the size and price bounds the server really
	// applied for this place: ranges are moved to bin edges, so the
	// effective bounds, not the requested ones, describe the population.
	BinApproximations map[string]json.RawMessage `json:"bin_approximations,omitempty"`

	Error *toolError `json:"error,omitempty"`
}

// comparePlace is one place as the text renderer sees it: the payload row,
// plus what the text needs and the payload does not carry.
type comparePlace struct {
	row compareRow
	// stat is the one stat row, when the place answered exactly one.
	stat *api.Stat
	// errText is errorText of a failed place, in the same words as a
	// failed market_stats call.
	errText string
}

func (s *server) comparePlaces(ctx context.Context, _ *mcp.CallToolRequest, args comparePlacesArgs) (*mcp.CallToolResult, any, error) {
	geoIDs, err := compareGeoIDs(args.GeoIDs)
	if err != nil {
		return fail(err)
	}

	c, err := s.client()
	if err != nil {
		return fail(err)
	}

	var (
		answer compareAnswer
		places []comparePlace
		metas  []api.Meta
	)
	for i, geoID := range geoIDs {
		params := api.StatsParams{GeoID: geoID}
		args.statsFilterArgs.apply(&params)

		resp, err := c.StatsCurrent(ctx, params)
		if !refusedLocally(err) {
			answer.Requests++
		}
		if err != nil {
			places = append(places, errorPlace(geoID, err))
			if code, stop := stopsComparison(ctx, err); stop {
				answer.Stopped = code
				answer.NotAsked = geoIDs[i+1:]
				break
			}
			continue
		}
		metas = append(metas, resp.Meta)
		if resp.Meta.Metered() {
			answer.MeteredCalls++
		}
		if answer.Filters == nil {
			filters := resp.Filters
			answer.Filters = &filters
		}
		places = append(places, answeredPlace(geoID, resp))
	}
	for _, place := range places {
		answer.Rows = append(answer.Rows, place.row)
	}
	answer.Currencies = distinctAnswered(answer.Rows, func(row compareRow) string { return row.Currency })

	text := compareText(answer, places) + compareCost(answer, metas)
	res := &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: answer,
	}
	// isError only when NO place answered: then there is nothing to quote,
	// and the agent must fix something first. With one answered place the
	// call succeeded in part, and an isError would make a client hide the
	// figures that were paid for.
	if !anyAnswered(answer.Rows) {
		res.IsError = true
	}
	return res, nil, nil
}

// compareGeoIDs trims the list, drops blanks, and checks its size before any
// request. A repeated geo_id is refused too: it would spend a metered call on
// an answer already in the comparison.
func compareGeoIDs(raw []string) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		// Case-blind: "ES" and "es" are the same country, and would be
		// paid for twice. The id is sent as written.
		key := strings.ToLower(id)
		if seen[key] {
			return nil, fmt.Errorf("geo_ids names %q twice; each place is one metered call, so name each place once", id)
		}
		seen[key] = true
		ids = append(ids, id)
	}
	if len(ids) < minComparePlaces || len(ids) > maxComparePlaces {
		return nil, fmt.Errorf("compare_places takes %d to %d geo_ids, got %d; nothing was sent. "+
			"For one place use market_stats; for more than %d, compare them in groups",
			minComparePlaces, maxComparePlaces, len(ids), maxComparePlaces)
	}
	return ids, nil
}

// stopsComparison reports whether an error means the NEXT place cannot be
// answered either, with the code to report.
//
//   - invalid_token, read_only_token, subscription_inactive: the same token
//     fails the same way for every place;
//   - quota_exhausted: the group is spent, every next call is a 402;
//   - rate_limited: the burst window outlasted the retry budget, so the next
//     call lands in the same window;
//   - a cancelled call: the agent gave up waiting;
//   - the tool call's deadline (ToolTimeout) ran out: there is no time left
//     for the next place;
//   - a filter value refused, locally or by the server (a parameter other
//     than geo_id): every place shares the filters, so every place would be
//     refused the same way.
//
// Everything else — an unknown place, a 500, a dropped connection — is about
// THIS request, and the next place may still answer.
func stopsComparison(ctx context.Context, err error) (string, bool) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timed_out", true
	}
	if ctx.Err() != nil {
		return "cancelled", true
	}
	if refusedLocally(err) {
		return "invalid_parameter", true
	}
	apiErr, isAPI := api.AsAPIError(err)
	if !isAPI {
		return "", false
	}
	switch apiErr.Code {
	case api.CodeInvalidToken, api.CodeReadOnlyToken, api.CodeSubscriptionInactive,
		api.CodeQuotaExhausted, api.CodeRateLimited:
		return string(apiErr.Code), true
	case api.CodeInvalidParameter, api.CodeMissingParameter:
		if apiErr.Parameter != "" && apiErr.Parameter != "geo_id" {
			return string(apiErr.Code), true
		}
	}
	return "", false
}

// refusedLocally reports an error the client raised before sending anything,
// so the place cost nothing and is not counted as a request.
func refusedLocally(err error) bool {
	var valErr *api.ValidationError
	return errors.As(err, &valErr)
}

// errorPlace is a failed place. The message is in the tools' words, like the
// text: structuredContent must not tell the agent to use --geo-id either.
func errorPlace(geoID string, err error) comparePlace {
	row := compareRow{GeoID: geoID, Error: errorPayload(err)}
	return comparePlace{row: row, errText: errorText(err)}
}

func answeredPlace(geoID string, resp *api.CurrentStats) comparePlace {
	var stat *api.Stat
	row := compareRow{
		GeoID:        geoID,
		Period:       resp.Period,
		Granularity:  resp.Granularity,
		WindowStart:  resp.WindowStart,
		WindowEnd:    resp.WindowEnd,
		AsOf:         resp.AsOf,
		Currency:     resp.Currency,
		Availability: resp.Availability,

		BinApproximations: resp.BinApproximations,
	}
	if p := resp.Territory.Place; p != nil {
		row.Name, row.Level, row.Country = p.Name, p.Level, p.Country
	}
	switch len(resp.Stats) {
	case 0:
	case 1:
		stat = &resp.Stats[0]
		row.MedianPricePerSqm = stat.MedianPricePerSqm
		row.MedianPrice = stat.MedianPrice
		row.MedianArea = stat.MedianArea
		row.P05PricePerSqm = stat.P05PricePerSqm
		row.P95PricePerSqm = stat.P95PricePerSqm
		row.Estimated = stat.Estimated
		row.LowConfidence = stat.LowConfidence
		if row.Name == "" && stat.Name != nil {
			row.Name = *stat.Name
		}
	default:
		row.Stats = resp.Stats
	}
	return comparePlace{row: row, stat: stat}
}

// anyAnswered reports whether any place came back without an error — with
// figures or with an availability verdict, both of which are answers.
func anyAnswered(rows []compareRow) bool {
	for _, row := range rows {
		if row.Error == nil {
			return true
		}
	}
	return false
}

// distinctAnswered is the sorted set of non-empty key values over the places
// that answered — the currencies or the periods. More than one currency means
// the figures are not comparable as printed; more than one period must be said.
func distinctAnswered(rows []compareRow, key func(compareRow) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, row := range rows {
		if v := key(row); row.Error == nil && v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func rowPeriod(row compareRow) string {
	if row.Period == nil {
		return ""
	}
	return *row.Period
}

// rowWindow is the period with its aggregation window, as the text cites it:
// "2026-08-01 (month, 2026-08-01 → 2026-08-31)". The parts the server did
// not send are left out, so an older server still gets "2026-08-01".
func rowWindow(row compareRow) string {
	text := api.DashPtr(row.Period)
	var parts []string
	if row.Granularity != "" {
		parts = append(parts, row.Granularity)
	}
	if row.WindowStart != nil || row.WindowEnd != nil {
		parts = append(parts, api.DashPtr(row.WindowStart)+" → "+api.DashPtr(row.WindowEnd))
	}
	if len(parts) > 0 {
		text += " (" + strings.Join(parts, ", ") + ")"
	}
	return text
}

// rowWindowKey is rowWindow for a place that named both ends of its window,
// "" otherwise — so a place that sent no window is not counted as a
// different one.
func rowWindowKey(row compareRow) string {
	if row.WindowStart == nil || row.WindowEnd == nil {
		return ""
	}
	return rowWindow(row)
}

func rowGranularity(row compareRow) string { return row.Granularity }

// differentWindows is the windows the answered places were aggregated over,
// when they differ: by their dates where the places sent both ends, else by
// granularity alone. Nil when they agree or nothing was said.
func differentWindows(rows []compareRow) []string {
	if windows := distinctAnswered(rows, rowWindowKey); len(windows) > 1 {
		return windows
	}
	if grans := distinctAnswered(rows, rowGranularity); len(grans) > 1 {
		return grans
	}
	return nil
}

func compareText(a compareAnswer, places []comparePlace) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Comparison of %d place(s), in the order asked.\n", len(a.Rows))
	if a.Filters != nil {
		b.WriteString(appliedFiltersText(*a.Filters))
		b.WriteString("\n")
	}
	b.WriteString(medianNote)
	b.WriteString("\n\n")

	for i, place := range places {
		b.WriteString(compareRowText(i, place))
	}

	if len(a.Currencies) > 1 {
		fmt.Fprintf(&b, "\n⚠️ The places answered in different currencies (%s). Do not compare these figures as printed — call again with currency set.\n",
			strings.Join(a.Currencies, ", "))
	}
	if periods := distinctAnswered(a.Rows, rowPeriod); len(periods) > 1 {
		fmt.Fprintf(&b, "\n⚠️ The places answered for different periods (%s). Say so when you compare them.\n",
			strings.Join(periods, ", "))
	} else if windows := differentWindows(a.Rows); len(windows) > 1 {
		// Same period date, different aggregation windows: the dates
		// match, so only this line shows the figures cover different spans.
		fmt.Fprintf(&b, "\n⚠️ The places share a period but were aggregated over different windows (%s). "+
			"Do not compare these figures as like for like — say which window each one covers.\n",
			strings.Join(windows, "; "))
	}
	var answered []api.Stat
	for _, place := range places {
		if place.stat != nil {
			answered = append(answered, *place.stat)
		}
	}
	if len(answered) > 0 {
		b.WriteString(statFootnotes(answered))
	}

	if a.Stopped != "" {
		fmt.Fprintf(&b, "\n⚠️ STOPPED after place %d of %d (%s): the next call could not succeed either.",
			len(a.Rows), len(a.Rows)+len(a.NotAsked), a.Stopped)
		if len(a.NotAsked) > 0 {
			fmt.Fprintf(&b, " Not asked: %s.", strings.Join(a.NotAsked, ", "))
		}
		b.WriteString(" The rows above are kept.\n")
	}
	return b.String()
}

// compareRowText is one place: its identity, then the core figures from
// coreFigures (the same words market_stats uses), the empty verdict and its
// move, or the error with its hint.
func compareRowText(i int, place comparePlace) string {
	text := compareRowCore(i, place)
	if approx := binApproxText(place.row.BinApproximations); approx != "" {
		text += "   " + approx
	}
	return text
}

func compareRowCore(i int, place comparePlace) string {
	row := place.row
	head := fmt.Sprintf("%d. ", i+1)
	if row.Name != "" {
		head += fmt.Sprintf("%s (%s, %s) — ", row.Name, api.Dash(row.Level), api.Dash(row.Country))
	}
	head += "geo_id " + row.GeoID

	switch {
	case row.Error != nil:
		return fmt.Sprintf("%s: ERROR — %s\n", head, indentLines(place.errText))
	case place.stat != nil:
		return fmt.Sprintf("%s · period %s: %s\n", head, rowWindow(row), figureLine(*place.stat, row.Currency))
	case len(row.Stats) > 1:
		return fmt.Sprintf("%s · period %s: %d rows — no single figure for the place; see structuredContent.rows[%d].stats\n",
			head, rowWindow(row), len(row.Stats), i)
	case row.Availability != nil:
		text := fmt.Sprintf("%s · period %s: no figures — %s\n", head, rowWindow(row), verdictText(row.Availability.AvailabilityVerdict))
		if a := row.Availability; a.EarliestNonemptyPeriod != nil || a.LatestNonemptyPeriod != nil {
			text += fmt.Sprintf("   This segment has data from %s to %s.\n", api.DashPtr(a.EarliestNonemptyPeriod), api.DashPtr(a.LatestNonemptyPeriod))
			if note := strings.TrimSpace(a.Note); note != "" {
				text += fmt.Sprintf("   Note: %s\n", note)
			}
		}
		if row.Availability.Status != "ok" {
			text += fmt.Sprintf("   What to do: %s\n", reasonMove(row.Availability.ReasonOrUnknown()))
		}
		return text
	default:
		return fmt.Sprintf("%s · period %s: no figures (the server did not say why)\n", head, rowWindow(row))
	}
}

// compareCost is the cost line for the whole comparison — one line, not one
// per place. It counts what the SERVER reported as metered, and says the
// usage after the last answer, so the agent sees what is left.
//
// ⚠️ AN ANSWER WITH NO QUOTA HEADERS IS NOT COUNTED AS FREE. It is named
// separately as unknown, the same rule as costText.
func compareCost(a compareAnswer, metas []api.Meta) string {
	line := fmt.Sprintf("\ncost: %d metered request(s) for %d place(s) asked", a.MeteredCalls, a.Requests)
	var unknown int
	var last *api.Meta
	for i := range metas {
		if !metas[i].HasQuotaHeaders() {
			unknown++
			continue
		}
		last = &metas[i]
	}
	if last != nil && last.Metered() {
		line += " " + last.QuotaDetail()
	}
	if unknown > 0 {
		line += fmt.Sprintf("; %d answer(s) carried no quota headers, so their cost is unknown", unknown)
	}
	if failed := a.Requests - len(metas); failed > 0 {
		line += fmt.Sprintf("; %d request(s) failed and are not counted here", failed)
	}
	return line + "\n"
}
