package mcpserver

import (
	"context"
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

A place that fails or is empty becomes a row saying why; the other places are still answered. The tool stops early only on a token, subscription, quota or rate-limit refusal, and then returns the rows it already has and how many calls it spent. ⚠️ Without currency each place answers in its own currency — pass currency to compare like with like. Price filters are USD whatever currency displays.`,
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
	// Requests is how many places were sent at all, whatever came back.
	MeteredCalls int `json:"metered_calls"`
	Requests     int `json:"requests"`
	// Stopped is set when the loop ended early: the code that stopped it,
	// and the geo_ids never asked about.
	Stopped    string   `json:"stopped,omitempty"`
	NotAsked   []string `json:"not_asked,omitempty"`
	Currencies []string `json:"currencies,omitempty"`
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

	Period   *string `json:"period,omitempty"`
	AsOf     *string `json:"as_of,omitempty"`
	Currency string  `json:"currency,omitempty"`

	MedianPricePerSqm *float64   `json:"median_price_per_sqm,omitempty"`
	MedianPrice       *float64   `json:"median_price,omitempty"`
	MedianArea        *float64   `json:"median_area,omitempty"`
	P05PricePerSqm    *float64   `json:"p05_price_per_sqm,omitempty"`
	P95PricePerSqm    *float64   `json:"p95_price_per_sqm,omitempty"`
	Estimated         bool       `json:"estimated,omitempty"`
	LowConfidence     *bool      `json:"low_confidence,omitempty"`
	Stats             []api.Stat `json:"stats,omitempty"`

	Availability *api.Availability `json:"availability,omitempty"`
	Filters      *api.Filters      `json:"filters,omitempty"`

	Error *compareError `json:"error,omitempty"`

	// stat and errText are the text renderer's view of the row; they are
	// not part of the payload.
	stat    *api.Stat
	errText string
}

type compareError struct {
	Code    string `json:"code,omitempty"`
	Status  int    `json:"status,omitempty"`
	Message string `json:"message"`
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
		metas  []api.Meta
	)
	for i, geoID := range geoIDs {
		params := api.StatsParams{GeoID: geoID}
		args.statsFilterArgs.apply(&params)

		answer.Requests++
		resp, err := c.StatsCurrent(ctx, params)
		if err != nil {
			answer.Rows = append(answer.Rows, errorRow(geoID, err))
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
		answer.Rows = append(answer.Rows, placeRow(geoID, resp))
	}
	answer.Currencies = rowCurrencies(answer.Rows)

	text := compareText(answer) + compareCost(answer, metas)
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
		if seen[id] {
			return nil, fmt.Errorf("geo_ids names %q twice; each place is one metered call, so name each place once", id)
		}
		seen[id] = true
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
//   - a cancelled call: the agent gave up waiting.
//
// Everything else — an unknown place, a bad filter value, a 500 — is about
// THIS request, and the next place may still answer.
func stopsComparison(ctx context.Context, err error) (string, bool) {
	if ctx.Err() != nil {
		return "cancelled", true
	}
	apiErr, isAPI := api.AsAPIError(err)
	if !isAPI {
		return "", false
	}
	switch apiErr.Code {
	case api.CodeInvalidToken, api.CodeReadOnlyToken, api.CodeSubscriptionInactive,
		api.CodeQuotaExhausted, api.CodeRateLimited:
		return string(apiErr.Code), true
	}
	return "", false
}

func errorRow(geoID string, err error) compareRow {
	row := compareRow{GeoID: geoID, errText: errorText(err)}
	row.Error = &compareError{Message: err.Error()}
	if apiErr, isAPI := api.AsAPIError(err); isAPI {
		row.Error.Code = string(apiErr.Code)
		row.Error.Status = apiErr.Status
	}
	return row
}

func placeRow(geoID string, resp *api.CurrentStats) compareRow {
	row := compareRow{
		GeoID:        geoID,
		Period:       resp.Period,
		AsOf:         resp.AsOf,
		Currency:     resp.Currency,
		Availability: resp.Availability,
		Filters:      &resp.Filters,
	}
	if p := resp.Territory.Place; p != nil {
		row.Name, row.Level, row.Country = p.Name, p.Level, p.Country
	}
	switch len(resp.Stats) {
	case 0:
	case 1:
		stat := resp.Stats[0]
		row.stat = &stat
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
	return row
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

// rowCurrencies is the set of currencies the answered places used, sorted.
// More than one means the figures are not comparable as printed.
func rowCurrencies(rows []compareRow) []string {
	seen := map[string]bool{}
	var out []string
	for _, row := range rows {
		if row.Error == nil && row.Currency != "" && !seen[row.Currency] {
			seen[row.Currency] = true
			out = append(out, row.Currency)
		}
	}
	sort.Strings(out)
	return out
}

func compareText(a compareAnswer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Comparison of %d place(s), in the order asked.\n", len(a.Rows))
	if f := firstFilters(a.Rows); f != nil {
		b.WriteString(appliedFiltersText(*f))
		b.WriteString("\n")
	}
	b.WriteString(medianNote)
	b.WriteString("\n\n")

	for i, row := range a.Rows {
		b.WriteString(compareRowText(i, row))
	}

	if len(a.Currencies) > 1 {
		fmt.Fprintf(&b, "\n⚠️ The places answered in different currencies (%s). Do not compare these figures as printed — call again with currency set.\n",
			strings.Join(a.Currencies, ", "))
	}
	if periods := rowPeriods(a.Rows); len(periods) > 1 {
		fmt.Fprintf(&b, "\n⚠️ The places answered for different periods (%s). Say so when you compare them.\n",
			strings.Join(periods, ", "))
	}
	var answered []api.Stat
	for _, row := range a.Rows {
		if row.stat != nil {
			answered = append(answered, *row.stat)
		}
	}
	if len(answered) > 0 {
		b.WriteString(statFootnotes(answered))
	}

	if a.Stopped != "" {
		fmt.Fprintf(&b, "\n⚠️ STOPPED after place %d of %d (%s): the next call would be refused the same way.",
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
func compareRowText(i int, row compareRow) string {
	head := fmt.Sprintf("%d. ", i+1)
	if row.Name != "" {
		head += fmt.Sprintf("%s (%s, %s) — ", row.Name, orDash(row.Level), orDash(row.Country))
	}
	head += "geo_id " + row.GeoID

	switch {
	case row.Error != nil:
		return fmt.Sprintf("%s: ERROR — %s\n", head, indentLines(row.errText))
	case row.stat != nil:
		mark := ""
		if row.stat.Estimated {
			mark = "~ "
		}
		line := fmt.Sprintf("%s · period %s: %s%s", head, strText(row.Period), mark,
			strings.Join(coreFigures(*row.stat, row.Currency), " · "))
		if row.LowConfidence != nil && *row.LowConfidence {
			line += " · low confidence"
		}
		return line + "\n"
	case len(row.Stats) > 1:
		return fmt.Sprintf("%s · period %s: %d rows — no single figure for the place; see structuredContent.rows[%d].stats\n",
			head, strText(row.Period), len(row.Stats), i)
	case row.Availability != nil:
		text := fmt.Sprintf("%s · period %s: no figures — %s\n", head, strText(row.Period), verdictText(row.Availability.AvailabilityVerdict))
		if row.Availability.Status != "ok" {
			text += fmt.Sprintf("   What to do: %s\n", reasonMove(row.Availability.ReasonOrUnknown()))
		}
		return text
	default:
		return fmt.Sprintf("%s · period %s: no figures (the server did not say why)\n", head, strText(row.Period))
	}
}

func firstFilters(rows []compareRow) *api.Filters {
	for _, row := range rows {
		if row.Filters != nil {
			return row.Filters
		}
	}
	return nil
}

// rowPeriods is the set of periods the answered places described, sorted.
func rowPeriods(rows []compareRow) []string {
	seen := map[string]bool{}
	var out []string
	for _, row := range rows {
		if row.Error == nil && row.Period != nil && !seen[*row.Period] {
			seen[*row.Period] = true
			out = append(out, *row.Period)
		}
	}
	sort.Strings(out)
	return out
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
		if last.QuotaUsed != nil {
			line += fmt.Sprintf(" (group %s, %d of %s used this cycle)", last.QuotaGroup, *last.QuotaUsed, last.QuotaLimit)
		} else {
			line += fmt.Sprintf(" (group %s, limit %s)", last.QuotaGroup, last.QuotaLimit)
		}
	}
	if unknown > 0 {
		line += fmt.Sprintf("; %d answer(s) carried no quota headers, so their cost is unknown", unknown)
	}
	if failed := a.Requests - len(metas); failed > 0 {
		line += fmt.Sprintf("; %d request(s) failed and are not counted here", failed)
	}
	return line + "\n"
}

// indentLines keeps a multi-line error (a hint, did_you_mean suggestions)
// under its row.
func indentLines(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n   ")
}
