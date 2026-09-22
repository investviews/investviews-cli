package commands

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/investviews/investviews-cli/internal/api"
)

// NewStatsCmd builds "investviews stats": the two METERED commands.
func NewStatsCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Market figures for a territory (METERED)",
		Long: `Market figures for a territory.

⚠️ THESE ARE THE ONLY TWO COMMANDS THAT SPEND QUOTA. Everything under "geo",
plus "coverage" and "usage", is free and uncounted, so resolve the place first
and spend a stats call once you know it holds data. Each run prints what it
cost, taken from the quota headers the server sent back.

"current" and "history" bill against SEPARATE budgets.`,
	}
	cmd.AddCommand(newStatsCurrentCmd(deps), newStatsHistoryCmd(deps))
	return cmd
}

// ⚠️ WHAT IS CHECKED LOCALLY, AND WHAT IS DELIBERATELY NOT.
//
// Only --h3 values are validated for content, and the rule that decides it is
// STRUCTURAL vs VOCABULARY:
//
//   - A cell id is a STRUCTURE. Its 64 bits carry a fixed layout — mode,
//     resolution, base cell, 15 digit slots — and no version of this API can
//     widen that layout. --h3 is also the only flag on this metered surface
//     fed by text the caller never typed — `--h3 -` reads standard input,
//     which is how a listing is piped in — so it is the one place rendered
//     prose can turn into a metered request. The layout is checked; see
//     api.IsH3Cell, including what it deliberately leaves to the server.
//     (The other stdin reader in this CLI is `auth login`, which sends
//     nothing anywhere.)
//   - --ad-type, --currency, --rooms, --ad-sub-type and --geo-id are
//     VOCABULARIES the server owns and v1 is additive-only, so a whitelist
//     copied in here would go stale the first time one grows and would then
//     refuse a request the API accepts. A stale local refusal is worse than a
//     server 400: the 400 is one wasted request, the refusal is a capability
//     the CLI has silently lost. The server's own message names the valid set;
//     /meta/filters publishes it.
//   - --res, --radius-km, --min-size and the rest are RANGES the plan and the
//     product set, not shapes. resolution_not_in_plan is per-account, so the
//     honest answer is the server's.
//
// Before adding a check here, ask which of the three it is.

// selectorFlags is the territory and the filters, shared by both stats
// commands so that one selector rule is written once.
type selectorFlags struct {
	geoID    string
	h3       []string
	lat      float64
	lng      float64
	radiusKm float64
	res      int

	adType    string
	adSubType string
	rooms     []string

	minSize     float64
	maxSize     float64
	minPriceUSD float64
	maxPriceUSD float64
	currency    string
}

func (s *selectorFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&s.geoID, "geo-id", "",
		"a place ref (R344953) or a two-letter country code (es) — one parameter, both kinds")
	f.StringArrayVar(&s.h3, "h3", nil,
		"H3 cells: repeat the flag, comma-join them, or pass - to read them from standard input "+
			"(pipe `geo hexes <id> --all --ids-only`, never the plain listing). "+
			"Ids are DECIMAL strings (613498076398616575), not 87… hex. "+
			"Every value is checked against the H3 bit layout BEFORE anything is sent, so a "+
			"word from a rendered listing is refused locally and free")
	f.Float64Var(&s.lat, "lat", 0, "circle centre latitude; requires --lng and --radius-km")
	f.Float64Var(&s.lng, "lng", 0, "circle centre longitude; requires --lat and --radius-km")
	f.Float64Var(&s.radiusKm, "radius-km", 0, "circle radius in kilometres, at most 500 (alias: --radius)")
	// ⚠️ --res BELONGS TO THE CIRCLE ALONE. It is refused alongside --h3
	// (cells state their own resolution) AND alongside --geo-id (a place is
	// answered as one row from its own boundary, so there is no cell size to
	// choose). Naming only --h3 here is what made the empty-answer hint
	// suggest "a coarser --res" to a --geo-id caller, whose next command
	// then failed locally.
	f.IntVar(&s.res, "res", 0,
		"H3 resolution 4..8, for the --lat/--lng/--radius-km circle ONLY; "+
			"refused alongside --h3 (its cells state their own) and alongside --geo-id "+
			"(a place is one row from its own boundary, with no cell size to choose)")

	f.StringVar(&s.adType, "ad-type", "",
		"asset class, e.g. real_estate_residential; see `investviews coverage --help` and /meta/filters")
	f.StringVar(&s.adSubType, "ad-sub-type", "", "buy (for sale) or rent (to let); the API defaults to buy")
	f.StringArrayVar(&s.rooms, "rooms", nil,
		"room bins: 1, 2, 3, 4, 5+, unknown. Repeatable or comma-joined. 'unknown' is a REAL bin")
	f.Float64Var(&s.minSize, "min-size", 0, "minimum living area, m²")
	f.Float64Var(&s.maxSize, "max-size", 0, "maximum living area, m²")
	f.Float64Var(&s.minPriceUSD, "min-price-usd", 0, "minimum price in USD — always USD, whatever --currency displays")
	f.Float64Var(&s.maxPriceUSD, "max-price-usd", 0, "maximum price in USD — always USD, whatever --currency displays")
	f.StringVar(&s.currency, "currency", "", "DISPLAY currency for the figures; it does not change which listings are selected")

	// --radius is accepted as an alias for --radius-km. The API's parameter
	// is radius_km and the unit belongs in the name, but --radius is the
	// short form people type; an alias is cheaper than a wrong unit.
	f.SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == "radius" {
			return pflag.NormalizedName("radius-km")
		}
		return pflag.NormalizedName(name)
	})
}

// params turns the flags into a request, refusing locally anything the API
// would refuse — so a mistake costs no request and no quota.
func (s *selectorFlags) params(cmd *cobra.Command, in io.Reader) (api.StatsParams, error) {
	cells, err := hexIDs(s.h3, in)
	if err != nil {
		return api.StatsParams{}, err
	}

	f := cmd.Flags()
	hasPoint := f.Changed("lat") || f.Changed("lng") || f.Changed("radius-km")

	// ⚠️ ONE TERRITORY PER REQUEST. The API enforces this too; catching it
	// here means the mistake is free. Two selectors have two different
	// answers and nothing can tell which one was meant.
	var named []string
	if strings.TrimSpace(s.geoID) != "" {
		named = append(named, "--geo-id")
	}
	if len(cells) > 0 {
		named = append(named, "--h3")
	}
	if hasPoint {
		named = append(named, "--lat/--lng/--radius-km")
	}
	switch {
	case len(named) == 0:
		return api.StatsParams{}, &api.ValidationError{
			Selectors: []string{"geo_id", "h3", "point"},
			Message: "name a territory with exactly one of --geo-id, --h3, or --lat/--lng/--radius-km. " +
				"Resolve a geo_id with `investviews geo search` or `investviews geo browse` — both are free",
		}
	case len(named) > 1:
		return api.StatsParams{}, &api.ValidationError{
			Parameter: strings.TrimPrefix(named[0], "--"),
			Selectors: named,
			Message: fmt.Sprintf("a request names ONE territory. Got %s — drop all but one. "+
				"Nothing was sent, so this cost no quota", strings.Join(named, " and ")),
		}
	case hasPoint && !(f.Changed("lat") && f.Changed("lng") && f.Changed("radius-km")):
		return api.StatsParams{}, &api.ValidationError{
			Selectors: []string{"lat", "lng", "radius_km"},
			Message:   "the circle selector needs all three of --lat, --lng and --radius-km",
		}
	}

	// ⚠️ CELLS ARE CHECKED BEFORE THEY ARE SPENT — all three input routes.
	// --h3 takes values from a repeated flag, a comma list and standard
	// input, and the last is RENDERED TEXT: a header line, an availability
	// sentence and a cost line all tokenise into words that sit in the list
	// looking like ids. Unchecked they go out on a METERED endpoint. Checked
	// here, the whole run costs nothing.
	if err := api.ValidateH3Cells(cells); err != nil {
		return api.StatsParams{}, err
	}

	params := api.StatsParams{
		GeoID:     strings.TrimSpace(s.geoID),
		H3:        cells,
		AdType:    s.adType,
		AdSubType: s.adSubType,
		Rooms:     splitList(s.rooms),
		Currency:  s.currency,
	}
	if hasPoint {
		params.Lat, params.Lng, params.RadiusKm = &s.lat, &s.lng, &s.radiusKm
	}
	if f.Changed("res") {
		params.Res = &s.res
	}
	if f.Changed("min-size") {
		params.MinSize = &s.minSize
	}
	if f.Changed("max-size") {
		params.MaxSize = &s.maxSize
	}
	if f.Changed("min-price-usd") {
		params.MinPriceUSD = &s.minPriceUSD
	}
	if f.Changed("max-price-usd") {
		params.MaxPriceUSD = &s.maxPriceUSD
	}
	return params, nil
}

// hexIDs expands the --h3 flag: repeats, comma lists, and "-" for standard
// input, which is how a `geo hexes … --ids-only` listing is piped straight in.
//
// ⚠️ It only SPLITS. Whether each token is a cell is decided after the
// one-territory guard, so a request naming two selectors is told to drop one
// rather than lectured about the contents of the one it is being told to drop.
func hexIDs(values []string, in io.Reader) ([]string, error) {
	var out []string
	for _, value := range values {
		if strings.TrimSpace(value) == "-" {
			piped, err := readList(in)
			if err != nil {
				return nil, err
			}
			if len(piped) == 0 {
				// An empty pipe would otherwise fall through as "no
				// territory was named", which is true and unhelpful: the
				// territory WAS named, by a listing that turned out to be
				// empty, and that is the thing to go and look at.
				return nil, &api.ValidationError{
					Parameter: "h3",
					Message: "--h3 - read standard input and found no cell ids. " +
						"`investviews geo hexes <geo_id> --ids-only` prints nothing when a place " +
						"holds no cells at a resolution this API publishes, and an empty list names " +
						"no territory. Nothing was sent, so this cost no quota",
				}
			}
			out = append(out, piped...)
			continue
		}
		out = append(out, splitList([]string{value})...)
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// stats current
// ─────────────────────────────────────────────────────────────────────────────

func newStatsCurrentCmd(deps Deps) *cobra.Command {
	var sel selectorFlags

	cmd := &cobra.Command{
		Use:   "current",
		Short: "Figures for the newest built period (METERED)",
		Long: `Market figures for a territory, from the newest BUILT period.

  investviews stats current --geo-id R344953
  investviews stats current --geo-id es --ad-type real_estate_commercial
  investviews geo hexes R344953 --all --ids-only | investviews stats current --h3 -

⚠️ --ids-only IS NOT OPTIONAL IN THAT PIPE. The plain "geo hexes" listing is
written for a reader — a header, an availability sentence and a cost line
around the ids — and piping it here splits those words up and sends them as
cells. Every --h3 value is now checked against the H3 bit layout before
anything is sent, so the plain pipe fails locally and free rather than at the
server; --ids-only is what makes it work.

⚠️ METERED, against the "current" group. Resolve the place first with the free
geo commands, and check its availability line before spending this.

⚠️ THE FIGURES DESCRIBE ONE PAST PERIOD, NEVER TODAY. The period, its window
and the as_of date are printed with every answer; cite them.

⚠️ Price filters are ALWAYS USD (--min-price-usd/--max-price-usd) whatever
--currency displays. --currency changes the display only; it does not change
which listings are selected.

⚠️ An empty answer is a 200 and a NORMAL answer: a covered place that held
nothing in this window. It is not a zero and not an error.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
	}
	v := addJSONFlag(cmd)
	sel.register(cmd)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		client, err := deps.client()
		if err != nil {
			return err
		}
		params, err := sel.params(cmd, cmd.InOrStdin())
		if err != nil {
			return err
		}
		resp, err := client.StatsCurrent(cmd.Context(), params)
		if err != nil {
			return err
		}
		return v.emit(cmd, resp, resp.Meta, 1, api.EndpointStatsCurrent, func(w io.Writer) {
			renderCurrent(w, resp, params)
		})
	}
	return cmd
}

// renderCurrent takes the params as well as the answer, because the empty
// answer has to suggest a next move and a next move is only valid for the
// selector that produced it.
func renderCurrent(w io.Writer, resp *api.CurrentStats, params api.StatsParams) {
	fmt.Fprintln(w, territoryLine(resp.Territory))
	fmt.Fprintf(w, "period %s (%s) · window %s → %s · as_of %s · fx %s · figures in %s\n",
		str(resp.Period), dash(resp.Granularity), str(resp.WindowStart), str(resp.WindowEnd),
		str(resp.AsOf), str(resp.FxDate), dash(resp.Currency))
	fmt.Fprintln(w, filtersLine(resp.Filters))
	fmt.Fprintln(w)

	if len(resp.Stats) == 0 {
		renderEmptyStats(w, resp, params)
		return
	}
	renderStatRows(w, resp.Stats, resp.Currency)
	renderStatFootnotes(w, resp.Stats)
}

// renderEmptyStats is the normal answer for a covered territory that held
// nothing this period. Every move it names must be one the CLI will accept for
// THE SELECTOR THAT PRODUCED THIS ANSWER — a hint is an instruction, and one
// that fails locally sends an agent into an error it did not cause.
//
// Two ways that went wrong, both fixed here:
//
//   - "a coarser --res" was printed unconditionally. --res is refused
//     alongside --geo-id AND alongside --h3 (see api.StatsParams.selector), so
//     it is only ever a move for the circle — and geo_id is the commonest
//     empty case there is.
//   - "investviews stats history" was printed with no selector whenever the
//     answer carried no geo_id, which is every --h3 and every circle request.
//     A bare `stats history` names no territory and is refused locally.
func renderEmptyStats(w io.Writer, resp *api.CurrentStats, params api.StatsParams) {
	if resp.Availability != nil {
		// The API says WHY, from a closed vocabulary — print its answer
		// rather than guessing one. The old guess ("it held nothing that met
		// the display floor") is wrong for two of the five reasons: an
		// unbuilt period, and filters that matched nothing.
		fmt.Fprintf(w, "No figures for this selection in period %s. That is a NORMAL answer, not an\n", str(resp.Period))
		fmt.Fprintln(w, "error and not a zero.")
		renderWhy(w, resp.Availability)
	} else {
		fmt.Fprintf(w, "No figures for this territory in period %s. That is a NORMAL answer, not an\n", str(resp.Period))
		fmt.Fprintln(w, "error and not a zero: the territory is covered, and it held nothing that met the")
		fmt.Fprintln(w, "display floor in this window.")
	}
	if resp.Snapshot.EarliestPeriod != "" {
		fmt.Fprintf(w, "History goes back to %s.\n", resp.Snapshot.EarliestPeriod)
	}

	var moves []string
	if sel := selectorArgs(params, resp.Territory); sel != "" {
		moves = append(moves, "investviews stats history "+sel)
	}
	if isCircle(params) {
		moves = append(moves, "a coarser --res")
	}
	moves = append(moves, "a wider filter set")
	fmt.Fprintf(w, "Next: %s.\n", strings.Join(moves, ", or "))
}

// anyRows reports whether any period of a series carries a figure.
func anyRows(series []api.HistoryPoint) bool {
	for _, point := range series {
		if len(point.Stats) > 0 {
			return true
		}
	}
	return false
}

// renderWhy prints the API's own reason for an empty answer, and the periods
// that DO hold data for this segment when the API names them. Those dates are
// the actionable half: they turn "empty" into a call that can succeed.
func renderWhy(w io.Writer, a *api.Availability) {
	fmt.Fprintf(w, "Why: %s", a.ReasonOrUnknown())
	if a.Message != "" {
		fmt.Fprintf(w, " — %s", a.Message)
	}
	fmt.Fprintln(w)
	if a.EarliestNonemptyPeriod != nil || a.LatestNonemptyPeriod != nil {
		fmt.Fprintf(w, "This segment has data from %s to %s", str(a.EarliestNonemptyPeriod), str(a.LatestNonemptyPeriod))
		if a.SearchedFrom != nil {
			fmt.Fprintf(w, " (searched back to %s)", *a.SearchedFrom)
		}
		fmt.Fprintln(w, ".")
	}
}

// maxHintCells is how many cell ids a hint will spell out before naming them
// by count instead. A hint that reprinted 500 ids would bury itself.
const maxHintCells = 8

// selectorArgs spells the territory THIS request named, so the follow-up
// command a hint prints is the one that just ran with "current" swapped for
// "history" — never a command missing its selector.
func selectorArgs(params api.StatsParams, territory api.Territory) string {
	switch {
	case strings.TrimSpace(params.GeoID) != "":
		return "--geo-id " + strings.TrimSpace(params.GeoID)
	case len(params.H3) > 0:
		if len(params.H3) <= maxHintCells {
			return "--h3 " + strings.Join(params.H3, ",")
		}
		return fmt.Sprintf("--h3 <the same %d cells>", len(params.H3))
	case isCircle(params):
		return fmt.Sprintf("--lat %g --lng %g --radius-km %g", *params.Lat, *params.Lng, *params.RadiusKm)
	case territory.GeoID != "":
		// Nothing was parsed from flags — the caller built the params
		// directly. Fall back to what the server echoed.
		return "--geo-id " + territory.GeoID
	default:
		return ""
	}
}

// isCircle reports the one selector that --res is valid for.
func isCircle(params api.StatsParams) bool {
	return params.Lat != nil && params.Lng != nil && params.RadiusKm != nil
}

// renderStatRows prints one row per cell or place. Each row's identity comes
// first, so it can be spent on the next call.
func renderStatRows(w io.Writer, stats []api.Stat, currency string) {
	fmt.Fprintf(w, "%d row(s).\n\n", len(stats))
	table(w, func(tw io.Writer) {
		fmt.Fprintf(tw, "ID\tNAME\tLEVEL\tMEDIAN %s/m²\tMEDIAN PRICE\tMEDIAN m²\tP05/m²\tP95/m²\tNOTES\n",
			dash(currency))
		for _, row := range stats {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				dash(row.H3), str(row.Name), str(row.ZoneLevel),
				num(row.MedianPricePerSqm), num(row.MedianPrice), num(row.MedianArea),
				num(row.P05PricePerSqm), num(row.P95PricePerSqm),
				statNotes(row))
		}
	})
}

// statNotes renders the two quality flags.
//
// ⚠️ low_confidence ABSENT IS NOT FALSE — the API stamps it only where a
// second period read is cheap, so an unstamped row says nothing either way and
// must not be rendered as "confident".
func statNotes(row api.Stat) string {
	var notes []string
	if row.Estimated {
		notes = append(notes, "estimated")
	}
	if row.LowConfidence != nil && *row.LowConfidence {
		notes = append(notes, "low-confidence")
	}
	if len(notes) == 0 {
		return "—"
	}
	return strings.Join(notes, ", ")
}

func renderStatFootnotes(w io.Writer, stats []api.Stat) {
	var estimated, stamped bool
	for _, row := range stats {
		estimated = estimated || row.Estimated
		stamped = stamped || row.LowConfidence != nil
	}
	if estimated {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "⚠️ \"estimated\" rows are NOT that cell's own figures: they are pooled from the")
		fmt.Fprintln(w, "cell and its six neighbours (~1.2 km). Quote them for the area, and never")
		fmt.Fprintln(w, "compare an estimated row against an exact one as if they measured the same thing.")
	}
	if !stamped {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "note: no row carries a low_confidence stamp. The API stamps it only where a")
		fmt.Fprintln(w, "second period read is cheap, so absent means UNSTAMPED — not \"confident\".")
	}
}

func territoryLine(t api.Territory) string {
	var b strings.Builder
	switch {
	case t.Place != nil:
		fmt.Fprintf(&b, "%s (%s, %s)", t.Place.Name, t.Place.Level, t.Place.Country)
		if t.GeoID != "" {
			fmt.Fprintf(&b, " — geo_id %s", t.GeoID)
		}
	case t.GeoID != "":
		fmt.Fprintf(&b, "geo_id %s", t.GeoID)
	case t.Center != nil:
		fmt.Fprintf(&b, "circle at %g, %g", t.Center.Lat, t.Center.Lng)
		if t.RadiusKm != nil {
			fmt.Fprintf(&b, " radius %g km", *t.RadiusKm)
		}
	default:
		fmt.Fprintf(&b, "%d cell(s)", t.HexCount)
	}
	fmt.Fprintf(&b, " · selector %s", dash(t.Selector))
	if t.Resolution != nil {
		fmt.Fprintf(&b, " · res %d", *t.Resolution)
	}
	if t.Grain != "" {
		fmt.Fprintf(&b, " · grain %s", t.Grain)
	}
	if t.HexCount > 0 && t.Selector != "" {
		fmt.Fprintf(&b, " · %d cell(s)", t.HexCount)
	}
	return b.String()
}

// filtersLine echoes the filter set AS APPLIED — defaults included, because a
// filter you did not name still has a value.
func filtersLine(f api.Filters) string {
	var b strings.Builder
	fmt.Fprintf(&b, "filters: %s / %s", dash(f.AdType), dash(f.AdSubType))
	if len(f.Rooms) > 0 {
		fmt.Fprintf(&b, " · rooms %s", strings.Join(f.Rooms, ","))
	}
	if f.MinSize != nil || f.MaxSize != nil {
		fmt.Fprintf(&b, " · size %s..%s m²", num(f.MinSize), num(f.MaxSize))
	}
	if f.MinPriceUSD != nil || f.MaxPriceUSD != nil {
		fmt.Fprintf(&b, " · price %s..%s USD", num(f.MinPriceUSD), num(f.MaxPriceUSD))
	}
	b.WriteString(" · price filters are USD whatever the display currency is")
	return b.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// stats history
// ─────────────────────────────────────────────────────────────────────────────

func newStatsHistoryCmd(deps Deps) *cobra.Command {
	var (
		sel      selectorFlags
		from, to string
	)

	cmd := &cobra.Command{
		Use:   "history",
		Short: "The same figures, period by period (METERED)",
		Long: `The same figures as "stats current", once per period across a range.

  investviews stats history --geo-id R344953 --from 2025-02 --to 2025-12

⚠️ METERED, against the "history" group — a SEPARATE budget from "current".

⚠️ POINTS ARE NOT ADDITIVE. Each point is an independent aggregate over its own
window, and at the rolling_3m granularity two adjacent windows overlap by two
months outright. Compare points and draw a line; never sum counts across
periods, and never read a sum as inventory.

--from omitted gives the LAST 12 PERIODS, not the whole history. A range longer
than 24 periods is refused, not truncated — page it yourself.

There is no --interval: the API derives the granularity (month or rolling_3m)
from the resolution and echoes it on every point.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
	}
	v := addJSONFlag(cmd)
	sel.register(cmd)
	cmd.Flags().StringVar(&from, "from", "", "earliest period, YYYY-MM or any date inside it; omitted, the last 12 periods")
	cmd.Flags().StringVar(&to, "to", "", "latest period, YYYY-MM or any date inside it; omitted, the newest built period")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		client, err := deps.client()
		if err != nil {
			return err
		}
		params, err := sel.params(cmd, cmd.InOrStdin())
		if err != nil {
			return err
		}
		resp, err := client.StatsHistory(cmd.Context(), api.HistoryParams{
			StatsParams: params, From: from, To: to,
		})
		if err != nil {
			return err
		}
		return v.emit(cmd, resp, resp.Meta, 1, api.EndpointStatsHistory, func(w io.Writer) {
			renderHistory(w, resp)
		})
	}
	return cmd
}

func renderHistory(w io.Writer, resp *api.HistoryResponse) {
	fmt.Fprintln(w, territoryLine(resp.Territory))
	fmt.Fprintf(w, "%d period(s) · figures in %s\n", len(resp.Series), dash(resp.Currency))
	fmt.Fprintln(w, filtersLine(resp.Filters))
	fmt.Fprintln(w)

	if len(resp.Series) == 0 {
		fmt.Fprintln(w, "No periods in this range. That is a NORMAL answer, not an error.")
		if resp.Availability != nil {
			renderWhy(w, resp.Availability)
		}
		if resp.Snapshot.EarliestPeriod != "" {
			fmt.Fprintf(w, "History goes back to %s — widen --from, or drop it for the last 12 periods.\n",
				resp.Snapshot.EarliestPeriod)
		}
		return
	}

	// One row per period is only honest while each period holds one row. A
	// multi-cell series has no single number per period, and averaging the
	// cells here would invent one.
	multi := false
	for _, point := range resp.Series {
		if len(point.Stats) > 1 {
			multi = true
			break
		}
	}

	table(w, func(tw io.Writer) {
		if multi {
			fmt.Fprintln(tw, "PERIOD\tGRANULARITY\tWINDOW\tAS_OF\tROWS")
			for _, point := range resp.Series {
				fmt.Fprintf(tw, "%s\t%s\t%s → %s\t%s\t%d\n",
					point.Period, point.Granularity, point.WindowStart, point.WindowEnd,
					point.AsOf, len(point.Stats))
			}
			return
		}
		fmt.Fprintf(tw, "PERIOD\tGRANULARITY\tWINDOW\tAS_OF\tMEDIAN %s/m²\tMEDIAN PRICE\tMEDIAN m²\n",
			dash(resp.Currency))
		for _, point := range resp.Series {
			var row api.Stat
			if len(point.Stats) == 1 {
				row = point.Stats[0]
			}
			fmt.Fprintf(tw, "%s\t%s\t%s → %s\t%s\t%s\t%s\t%s\n",
				point.Period, point.Granularity, point.WindowStart, point.WindowEnd, point.AsOf,
				num(row.MedianPricePerSqm), num(row.MedianPrice), num(row.MedianArea))
		}
	})

	if multi {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "A period here holds several cells, so there is no single figure per period.")
		fmt.Fprintln(w, "Use --json for the per-cell figures, or ask about one cell at a time.")
	}
	// A series whose every period came back empty is a row of dashes; the
	// reason is the only line in it an agent can act on.
	if resp.Availability != nil && !anyRows(resp.Series) {
		fmt.Fprintln(w)
		renderWhy(w, resp.Availability)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "⚠️ POINTS ARE NOT ADDITIVE — compare them, never sum them.")
	if note := strings.TrimSpace(resp.SeriesNote); note != "" {
		fmt.Fprintln(w, note)
	}
}
