package mcpserver

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/investviews/investviews-cli/internal/api"
)

// The metered stats tools: market_stats and price_history. They are the only tools that spend quota, and the
// only ones whose answers a user will quote — so their text names the
// statistic every time, and an empty answer says WHY it is empty.
//
// ⚠️ EVERY FIGURE IS A MEDIAN OF ASKING PRICES. Not a sale price, not the price
// of one flat, not today's price. The text says "median asking price per m²",
// "median asking price" and "median size" — never "price" — because an agent
// copies the words it was given, and the user reads them as a valuation.
//
// The argument structs are built from two halves so the later stats tools
// reuse them: territoryArgs (WHERE — one of three selectors) and
// statsFilterArgs (WHICH listings). market_stats and price_history take both;
// compare_places takes a list of geo_ids and the filters alone.

func (s *server) registerStats(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "market_stats",
		Annotations: readOnly("Market figures for one place"),
		Description: `METERED — one call against the "current" quota group. The newest built period's figures for ONE territory: median asking price per m², median asking price, median size, and the p05–p95 spread.

Name the territory with EXACTLY ONE of:
- geo_id — a place from find_place / browse_places, or an ISO country code (es);
- h3 — one or more H3 cells, all at one resolution (from place_at_point);
- lat + lng + radius_km — a circle (res optional, for this selector only).
Two selectors are refused before any request is sent, so the mistake costs nothing.

Resolve the place with the free tools first and spend this once. The figures describe one PERIOD (cite period, window and as_of), never today. An empty answer is normal, not a zero: its availability reason says what to do next. ⚠️ Price filters are USD whatever currency displays.`,
	}, s.marketStats)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "price_history",
		Annotations: readOnly("Market figures period by period"),
		Description: `METERED — one call against the "history" quota group, a SEPARATE budget from "current". The same figures as market_stats, once per period across a range: median asking price per m², median asking price and median size for each period.

Name the territory exactly as for market_stats: ONE of geo_id, h3, or lat + lng + radius_km (res for the circle only). Two selectors are refused before any request is sent.

from and to are a period (2025-02) or any date inside one. from omitted gives the LAST 12 PERIODS, not the whole history; to omitted is the newest built period. A range longer than 24 periods is REFUSED, not truncated — split it into ranges of 24 periods or fewer, one call each.

⚠️ POINTS ARE NOT ADDITIVE. Each point is an independent aggregate over its own window, and rolling_3m windows overlap by two months. Compare points and draw a line; never sum them. Use this, not market_stats, when market_stats came back empty with period_not_built.`,
	}, s.priceHistory)
}

// ─────────────────────────────────────────────────────────────────────────────
// shared arguments — market_stats, price_history, compare_places
// ─────────────────────────────────────────────────────────────────────────────

// territoryArgs is WHERE: exactly one of geo_id, h3, or lat+lng+radius_km.
//
// ⚠️ NONE OF THEM IS REQUIRED IN THE SCHEMA, ON PURPOSE. They are alternatives,
// and JSON Schema's "required" cannot say "exactly one of". The check lives in
// api.StatsParams.selector() — one rule, shared with the CLI — and it refuses
// before any request, so a wrong combination costs nothing.
//
// Lat, Lng and RadiusKm are pointers: 0 is a real coordinate, so "not set"
// must differ from 0, and a half-named circle must be caught as such.
type territoryArgs struct {
	GeoID    string   `json:"geo_id,omitempty" jsonschema:"a place's geo_id from find_place or browse_places (R344953), or an ISO country code (es)"`
	H3       []string `json:"h3,omitempty" jsonschema:"H3 cell ids, all at one resolution, e.g. from place_at_point"`
	Lat      *float64 `json:"lat,omitempty" jsonschema:"circle centre latitude; needs lng and radius_km too"`
	Lng      *float64 `json:"lng,omitempty" jsonschema:"circle centre longitude; needs lat and radius_km too"`
	RadiusKm *float64 `json:"radius_km,omitempty" jsonschema:"circle radius in km; needs lat and lng too"`
	Res      *int     `json:"res,omitempty" jsonschema:"H3 resolution for the circle selector only; refused with geo_id and with h3"`
}

// statsFilterArgs is WHICH listings. Every field is optional and an unset one
// is not sent: the API applies its own default (ad_sub_type defaults to buy)
// and echoes the filters as applied, which the text repeats.
type statsFilterArgs struct {
	AdType      string   `json:"ad_type,omitempty" jsonschema:"asset class, e.g. real_estate_residential or real_estate_commercial; the filters tool lists them"`
	AdSubType   string   `json:"ad_sub_type,omitempty" jsonschema:"buy (sale asking prices, the default) or rent"`
	Rooms       []string `json:"rooms,omitempty" jsonschema:"room counts, e.g. [\"2\",\"3\"] or [\"5+\"]; the filters tool lists them"`
	MinSize     *float64 `json:"min_size,omitempty" jsonschema:"smallest size in m², inclusive"`
	MaxSize     *float64 `json:"max_size,omitempty" jsonschema:"largest size in m², inclusive"`
	MinPriceUSD *float64 `json:"min_price_usd,omitempty" jsonschema:"lowest asking price, ALWAYS in USD whatever currency displays"`
	MaxPriceUSD *float64 `json:"max_price_usd,omitempty" jsonschema:"highest asking price, ALWAYS in USD whatever currency displays"`
	Currency    string   `json:"currency,omitempty" jsonschema:"display currency for the figures (EUR, USD, …); does not change which listings are selected"`
}

// params is the territory half of api.StatsParams. Blank cell ids are dropped
// so a stray "" does not turn into a cell the API is asked to price.
func (t territoryArgs) params() api.StatsParams {
	var cells []string
	for _, cell := range t.H3 {
		if cell = strings.TrimSpace(cell); cell != "" {
			cells = append(cells, cell)
		}
	}
	return api.StatsParams{
		GeoID: strings.TrimSpace(t.GeoID), H3: cells,
		Lat: t.Lat, Lng: t.Lng, RadiusKm: t.RadiusKm, Res: t.Res,
	}
}

// apply sets the filter half of p.
func (f statsFilterArgs) apply(p *api.StatsParams) {
	p.AdType = f.AdType
	p.AdSubType = f.AdSubType
	p.Rooms = f.Rooms
	p.MinSize, p.MaxSize = f.MinSize, f.MaxSize
	p.MinPriceUSD, p.MaxPriceUSD = f.MinPriceUSD, f.MaxPriceUSD
	p.Currency = f.Currency
}

// isCircle reports the one selector res is valid for — and so the only one an
// empty answer may suggest "a coarser res" to.
func (t territoryArgs) isCircle() bool {
	return t.Lat != nil && t.Lng != nil && t.RadiusKm != nil
}

// maxSelectorCells is how many cell ids a next-move line spells out before
// naming them by count. A hint that reprinted 500 ids would bury itself.
const maxSelectorCells = 8

// selectorText spells the territory THIS request named, in argument form, so
// a next move ("price_history with geo_id …") is the call that just ran with
// one tool swapped — never a call missing its selector.
func (t territoryArgs) selectorText(territory api.Territory) string {
	p := t.params()
	switch {
	case p.GeoID != "":
		return fmt.Sprintf("geo_id %q", p.GeoID)
	case len(p.H3) > maxSelectorCells:
		return fmt.Sprintf("h3 = the same %d cells", len(p.H3))
	case len(p.H3) > 0:
		return fmt.Sprintf("h3 [%s]", quoteList(p.H3))
	case t.isCircle():
		return fmt.Sprintf("lat %g, lng %g, radius_km %g", *t.Lat, *t.Lng, *t.RadiusKm)
	case territory.GeoID != "":
		return fmt.Sprintf("geo_id %q", territory.GeoID)
	default:
		return ""
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// market_stats
// ─────────────────────────────────────────────────────────────────────────────

type marketStatsArgs struct {
	territoryArgs
	statsFilterArgs
}

func (s *server) marketStats(ctx context.Context, _ *mcp.CallToolRequest, args marketStatsArgs) (*mcp.CallToolResult, any, error) {
	params := args.territoryArgs.params()
	args.statsFilterArgs.apply(&params)

	c, err := s.client()
	if err != nil {
		return fail(err)
	}
	// StatsCurrent checks the selector itself, before any request: two
	// selectors, none, or a half-named circle never reach the meter.
	resp, err := c.StatsCurrent(ctx, params)
	if err != nil {
		return fail(err)
	}
	return ok(resp, currentText(args.territoryArgs, resp), resp.Meta)
}

// maxTextRows is how many stat rows the text spells out. An h3 request can
// return hundreds of cells; the text is a summary, and every row is in
// structuredContent either way.
const maxTextRows = 25

func currentText(args territoryArgs, resp *api.CurrentStats) string {
	var b strings.Builder
	b.WriteString(territoryText(resp.Territory))
	b.WriteString("\n")
	fmt.Fprintf(&b, "period %s (%s) · window %s → %s · as_of %s · figures in %s (fx %s)\n",
		strText(resp.Period), orDash(resp.Granularity), strText(resp.WindowStart), strText(resp.WindowEnd),
		strText(resp.AsOf), orDash(resp.Currency), strText(resp.FxDate))
	b.WriteString(appliedFiltersText(resp.Filters))
	b.WriteString("\n\n")

	if len(resp.Stats) == 0 {
		fmt.Fprintf(&b, "No figures for this selection in period %s. That is a normal answer — not an error, and not a price of zero.\n",
			strText(resp.Period))
		if resp.Availability != nil {
			b.WriteString(availabilityText(resp.Availability))
		}
		if resp.Snapshot.EarliestPeriod != "" {
			fmt.Fprintf(&b, "History goes back to %s.\n", resp.Snapshot.EarliestPeriod)
		}
		b.WriteString(emptyMovesText(args, resp.Territory))
		return b.String()
	}

	fmt.Fprintf(&b, "%d row(s). %s\n", len(resp.Stats), medianNote)
	for i, row := range resp.Stats {
		if i == maxTextRows {
			fmt.Fprintf(&b, "\n… %d more row(s) in structuredContent.stats.\n", len(resp.Stats)-maxTextRows)
			break
		}
		b.WriteString("\n")
		b.WriteString(statRowText(row, resp.Currency))
	}
	b.WriteString(statFootnotes(resp.Stats))
	return b.String()
}

// medianNote is said once above the rows of every stats answer.
const medianNote = "Every figure is a median of ASKING prices of listings in this place and period — " +
	"not a sale price, not one flat's price. Quote it as \"median asking price\", never as \"price\"."

// emptyMovesText names the next calls that can succeed for THIS selector.
// "A coarser res" only for the circle: res is refused with geo_id and h3, and
// a hint naming an argument the tool then refuses sends the agent into an
// error it did not cause.
func emptyMovesText(args territoryArgs, territory api.Territory) string {
	var moves []string
	if sel := args.selectorText(territory); sel != "" {
		moves = append(moves, "price_history with "+sel)
	}
	if args.isCircle() {
		moves = append(moves, "a coarser res")
	}
	moves = append(moves, "wider filters")
	return "Next: " + strings.Join(moves, ", or ") + ".\n"
}

// ─────────────────────────────────────────────────────────────────────────────
// price_history
// ─────────────────────────────────────────────────────────────────────────────

// priceHistoryArgs is market_stats' arguments plus the period range. from and
// to are passed through as given: the API accepts a period or any date inside
// one, and clamps an omitted bound to what exists.
type priceHistoryArgs struct {
	territoryArgs
	statsFilterArgs
	From string `json:"from,omitempty" jsonschema:"earliest period, YYYY-MM or any date inside it; omitted, the last 12 periods"`
	To   string `json:"to,omitempty" jsonschema:"latest period, YYYY-MM or any date inside it; omitted, the newest built period"`
}

// maxHistoryPeriods is the API's cap on one /stats/history range. It is only
// QUOTED here, in the split-the-range note: the API is the authority, and a
// local check would refuse a range the server may one day accept.
const maxHistoryPeriods = 24

func (s *server) priceHistory(ctx context.Context, _ *mcp.CallToolRequest, args priceHistoryArgs) (*mcp.CallToolResult, any, error) {
	params := api.HistoryParams{
		StatsParams: args.territoryArgs.params(),
		From:        strings.TrimSpace(args.From),
		To:          strings.TrimSpace(args.To),
	}
	args.statsFilterArgs.apply(&params.StatsParams)

	c, err := s.client()
	if err != nil {
		return fail(err)
	}
	resp, err := c.StatsHistory(ctx, params)
	if err != nil {
		if rangeRefused(err) {
			return failNote(err, splitRangeNote)
		}
		return fail(err)
	}
	return ok(resp, historyText(args, resp), resp.Meta)
}

// splitRangeNote is the move for a refused range. The API's own message says
// WHAT is wrong; this says what the next calls are.
var splitRangeNote = fmt.Sprintf("price_history answers at most %d periods per call, and a longer range is "+
	"refused, not truncated. Split from..to into ranges of %d periods or fewer and call once per range "+
	"(each call is one metered request against the history group).", maxHistoryPeriods, maxHistoryPeriods)

// rangeRefused reports the 422 /stats/history sends for a range over the cap.
//
// ⚠️ THE CONTRACT GIVES THIS REFUSAL NO CODE OF ITS OWN. It is a 422 that is
// neither too_many_hexes (the cell budget, with its own hint) nor a report
// code, so it is recognised by status plus "not one of those" — or by the
// parameter it names, when the server names from or to.
func rangeRefused(err error) bool {
	apiErr, isAPI := api.AsAPIError(err)
	if !isAPI {
		return false
	}
	if apiErr.Parameter == "from" || apiErr.Parameter == "to" {
		return true
	}
	return apiErr.Status == 422 &&
		apiErr.Code != api.CodeTooManyHexes &&
		apiErr.Code != api.CodeReportsUnavailableForCountry
}

func historyText(args priceHistoryArgs, resp *api.HistoryResponse) string {
	var b strings.Builder
	b.WriteString(territoryText(resp.Territory))
	b.WriteString("\n")
	fmt.Fprintf(&b, "%d period(s) · figures in %s\n", len(resp.Series), orDash(resp.Currency))
	b.WriteString(appliedFiltersText(resp.Filters))
	b.WriteString("\n\n")

	if len(resp.Series) == 0 {
		b.WriteString("No periods in this range. That is a normal answer — not an error, and not a price of zero.\n")
		if resp.Availability != nil {
			b.WriteString(availabilityText(resp.Availability))
		}
		if resp.Snapshot.EarliestPeriod != "" {
			fmt.Fprintf(&b, "History goes back to %s — move from earlier, or drop it for the last 12 periods.\n",
				resp.Snapshot.EarliestPeriod)
		}
		return b.String()
	}

	b.WriteString(medianNote)
	b.WriteString("\n\n")
	for i, point := range resp.Series {
		b.WriteString(historyPointText(i, point, resp.Currency))
	}

	// A series whose every period came back empty is a column of dashes; the
	// reason is the only line in it an agent can act on.
	if resp.Availability != nil && !anyPointRows(resp.Series) {
		b.WriteString("\n")
		b.WriteString(availabilityText(resp.Availability))
	}
	b.WriteString(seriesFootnotes(resp.Series))
	b.WriteString("\n⚠️ POINTS ARE NOT ADDITIVE — each is an independent aggregate over its own window. Compare them, never sum them.\n")
	if note := strings.TrimSpace(resp.SeriesNote); note != "" {
		b.WriteString(note)
		b.WriteString("\n")
	}
	return b.String()
}

// historyPointText is one line per period, led by the period and its window
// so the agent cites WHICH window a figure describes.
//
// ⚠️ ONE LINE IS ONLY HONEST WHILE A PERIOD HOLDS ONE ROW. A multi-cell series
// has no single figure per period, and averaging the cells here would invent
// one — so such a period names its row count and points at the payload.
func historyPointText(i int, point api.HistoryPoint, currency string) string {
	head := fmt.Sprintf("- %s (%s, window %s → %s, as_of %s)", orDash(point.Period), orDash(point.Granularity),
		orDash(point.WindowStart), orDash(point.WindowEnd), orDash(point.AsOf))
	switch len(point.Stats) {
	case 0:
		if point.Availability != nil {
			return fmt.Sprintf("%s: no figures — %s\n", head, verdictText(*point.Availability))
		}
		return head + ": no figures\n"
	case 1:
		row := point.Stats[0]
		mark := ""
		if row.Estimated {
			mark = "~ "
		}
		line := fmt.Sprintf("%s: %s%s", head, mark, strings.Join(coreFigures(row, currency), " · "))
		if row.LowConfidence != nil && *row.LowConfidence {
			line += " · low confidence"
		}
		return line + "\n"
	default:
		return fmt.Sprintf("%s: %d cells — no single figure for the period; per-cell figures in structuredContent.series[%d].stats\n",
			head, len(point.Stats), i)
	}
}

// anyPointRows reports whether any period of a series carries a figure.
func anyPointRows(series []api.HistoryPoint) bool {
	for _, point := range series {
		if len(point.Stats) > 0 {
			return true
		}
	}
	return false
}

// seriesFootnotes is statFootnotes over every period's rows, said once.
func seriesFootnotes(series []api.HistoryPoint) string {
	var rows []api.Stat
	for _, point := range series {
		rows = append(rows, point.Stats...)
	}
	if len(rows) == 0 {
		return ""
	}
	return statFootnotes(rows)
}

// ─────────────────────────────────────────────────────────────────────────────
// words — shared by market_stats, price_history and compare_places
// ─────────────────────────────────────────────────────────────────────────────

// territoryText is the territory as the server echoed it: what was ACTUALLY
// asked about, which is what the agent must cite.
func territoryText(t api.Territory) string {
	var b strings.Builder
	switch {
	case t.Place != nil:
		fmt.Fprintf(&b, "%s (%s, %s)", orDash(t.Place.Name), orDash(t.Place.Level), orDash(t.Place.Country))
		if t.GeoID != "" {
			fmt.Fprintf(&b, " — geo_id %s", t.GeoID)
		}
	case t.GeoID != "":
		fmt.Fprintf(&b, "geo_id %s", t.GeoID)
	case t.Center != nil:
		fmt.Fprintf(&b, "circle at %g, %g", t.Center.Lat, t.Center.Lng)
		if t.RadiusKm != nil {
			fmt.Fprintf(&b, ", radius %g km", *t.RadiusKm)
		}
	default:
		fmt.Fprintf(&b, "%d cell(s)", t.HexCount)
	}
	fmt.Fprintf(&b, " · selector %s", orDash(t.Selector))
	if t.Resolution != nil {
		fmt.Fprintf(&b, " · res %d", *t.Resolution)
	}
	if t.Grain != "" {
		fmt.Fprintf(&b, " · grain %s", t.Grain)
	}
	return b.String()
}

// appliedFiltersText echoes the filters AS APPLIED — defaults included, because a
// filter the agent did not name still has a value, and "buy" vs "rent" is the
// difference between a sale price and a monthly rent.
func appliedFiltersText(f api.Filters) string {
	var b strings.Builder
	fmt.Fprintf(&b, "filters: %s / %s", orDash(f.AdType), orDash(f.AdSubType))
	if len(f.Rooms) > 0 {
		fmt.Fprintf(&b, " · rooms %s", strings.Join(f.Rooms, ","))
	}
	if f.MinSize != nil || f.MaxSize != nil {
		fmt.Fprintf(&b, " · size %s..%s m²", numText(f.MinSize), numText(f.MaxSize))
	}
	if f.MinPriceUSD != nil || f.MaxPriceUSD != nil {
		fmt.Fprintf(&b, " · asking price %s..%s USD", numText(f.MinPriceUSD), numText(f.MaxPriceUSD))
	}
	b.WriteString(" · price filters are USD whatever the display currency is")
	return b.String()
}

// statRowText is one row: its identity first (so it can be spent on the next
// call), then the three core figures by their full names, then the spread
// and the quality flags.
func statRowText(row api.Stat, currency string) string {
	var b strings.Builder
	name := strText(row.Name)
	if row.Estimated {
		// "~" is the mark the instructions tell the agent to carry into
		// its answer for anything that is not the place's own figure.
		name = "~ " + name
	}
	fmt.Fprintf(&b, "- %s", name)
	if row.ZoneLevel != nil && *row.ZoneLevel != "" {
		fmt.Fprintf(&b, " — figures hold at level %s", *row.ZoneLevel)
	}
	if row.H3 != "" {
		fmt.Fprintf(&b, " — h3 %s", row.H3)
	}
	b.WriteString("\n")
	for _, line := range coreFigures(row, currency) {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	if row.P05PricePerSqm != nil || row.P95PricePerSqm != nil {
		fmt.Fprintf(&b, "  p05–p95 asking price per m²: %s – %s\n",
			money(row.P05PricePerSqm, currency), money(row.P95PricePerSqm, currency))
	}
	if notes := statNotes(row); notes != "" {
		fmt.Fprintf(&b, "  notes: %s\n", notes)
	}
	return b.String()
}

// coreFigures is the three columns every answer and every comparison row
// carries, by their full names. compare_places builds its rows from it, so a
// comparison can never word a figure differently from market_stats.
func coreFigures(row api.Stat, currency string) []string {
	return []string{
		"median asking price per m²: " + money(row.MedianPricePerSqm, currency),
		"median asking price: " + money(row.MedianPrice, currency),
		"median size: " + area(row.MedianArea),
	}
}

// statNotes renders the two quality flags.
//
// ⚠️ low_confidence ABSENT IS NOT FALSE — the API stamps it only where a
// second period read is cheap. An unstamped row gets no note here; the
// footnote says once what that means.
func statNotes(row api.Stat) string {
	var notes []string
	if row.Estimated {
		notes = append(notes, "~ estimated — pooled from this cell and its six neighbours (~1.2 km); quote it for the area")
	}
	if row.LowConfidence != nil && *row.LowConfidence {
		notes = append(notes, "low confidence")
	}
	return strings.Join(notes, "; ")
}

func statFootnotes(stats []api.Stat) string {
	var estimated, stamped bool
	for _, row := range stats {
		estimated = estimated || row.Estimated
		stamped = stamped || row.LowConfidence != nil
	}
	var b strings.Builder
	if estimated {
		b.WriteString("\n⚠️ Rows marked ~ are NOT that cell's own figures: they are pooled from the cell and its six neighbours. Never compare an estimated row with an exact one as if they measured the same thing.\n")
	}
	if !stamped {
		b.WriteString("\nNo row carries a low_confidence stamp. Absent means UNSTAMPED, not \"confident\".\n")
	}
	return b.String()
}

// availabilityText is the API's own answer to "why is this empty", and WHEN
// this segment does hold data. The dates are the actionable half: they turn
// "empty" into a call that can succeed.
//
// It ends with the move for the reason, from the same table the server
// instructions give — so a reason seen here and a reason read there lead to
// the same next call.
func availabilityText(a *api.Availability) string {
	var b strings.Builder
	b.WriteString(verdictText(a.AvailabilityVerdict))
	b.WriteString("\n")
	if a.EarliestNonemptyPeriod != nil || a.LatestNonemptyPeriod != nil {
		fmt.Fprintf(&b, "This segment has data from %s to %s", strText(a.EarliestNonemptyPeriod), strText(a.LatestNonemptyPeriod))
		if a.SearchedFrom != nil {
			fmt.Fprintf(&b, " (searched back to %s)", *a.SearchedFrom)
		}
		b.WriteString(".\n")
	} else if a.SearchedFrom != nil && a.Status != "ok" {
		// A null date is "nothing in the window we looked at", never
		// "nothing, ever" — say which window.
		fmt.Fprintf(&b, "No period with data since %s (an earlier one may still hold some).\n", *a.SearchedFrom)
	}
	if a.Status != "ok" {
		fmt.Fprintf(&b, "What to do: %s\n", reasonMove(a.ReasonOrUnknown()))
	}
	return b.String()
}

// verdictText is the one-line form — machine words first — for a history
// point or a comparison row, where the dates are stated elsewhere.
func verdictText(v api.AvailabilityVerdict) string {
	if v.Status == "ok" {
		return "availability: ok"
	}
	text := fmt.Sprintf("availability: %s — %s", orDash(v.Status), v.ReasonOrUnknown())
	if v.Message != "" {
		text += ": " + v.Message
	}
	return text
}

// reasonMove is the next step for each reason in the API's closed vocabulary.
// An unknown reason is read as not_determined — the API's own instruction —
// never as an error.
//
// ⚠️ THE REASON DESCRIBES OUR AGGREGATE, NEVER THE MARKET. None of these
// moves says "nothing is for sale there".
func reasonMove(reason string) string {
	switch reason {
	case "no_data_for_selection":
		return "the place holds data, but not for these filters — widen them (rooms, size, price)."
	case "period_not_built":
		return "this period is not built yet — use price_history for the periods that are."
	case "no_data_for_place":
		return "we hold no figures for this place — pick another place; this one will not answer."
	case "below_minimum_sample":
		return "too few listings to publish a figure — try price_history, or a wider area (the parent place)."
	default:
		return "the API could not say why — try price_history, or a wider area (the parent place)."
	}
}

// numText is a number rounded to cents, with no trailing zeros. nil is "—":
// the aggregate held nothing for that field, which is not 0.
func numText(v *float64) string {
	if v == nil {
		return "—"
	}
	return strconv.FormatFloat(math.Round(*v*100)/100, 'f', -1, 64)
}

func money(v *float64, currency string) string {
	if v == nil {
		return "—"
	}
	return numText(v) + " " + orDash(currency)
}

func area(v *float64) string {
	if v == nil {
		return "—"
	}
	return numText(v) + " m²"
}

func quoteList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = strconv.Quote(item)
	}
	return strings.Join(quoted, ", ")
}
