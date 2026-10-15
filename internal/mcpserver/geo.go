package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/investviews/investviews-cli/internal/api"
)

// The three free geography tools. They are how an agent turns words ("Valencia",
// "the old town", a pin on a map) into a geo_id or a cell it can spend on the
// metered stats tools — and how it learns, before spending, whether there is
// anything there to buy.
//
// ⚠️ EVERY ROW'S TEXT CARRIES THE THREE-STATE SIGNAL, NOT JUST THE NAME. A hit
// with prices but nothing this period is a market_stats call that answers
// no_data — and a price_history call that may well answer. An agent that sees
// only the name spends the wrong call, or gives up on a live place.

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

func (s *server) registerGeo(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "find_place",
		Annotations: readOnly("Find a place by name"),
		Description: `FREE — costs no quota. Resolve a place name (city, district, neighbourhood) to a geo_id you can pass to market_stats, price_history or compare_places.

Names are not unique: many neighbourhood names repeat inside one country. Tell same-named hits apart by the ancestry chain under each, never by the name. Every hit says whether the place has prices and data this period; do not spend a metered call on a dead end.

There is no paging: one answer, at most 50 hits (limit defaults to 10). A miss is an error that lists did_you_mean suggestions, each with a geo_id you can use straight away.`,
	}, s.findPlace)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "browse_places",
		Annotations: readOnly("Browse the geography"),
		Description: fmt.Sprintf(`FREE — costs no quota. Walk the place tree one page at a time: no parent lists the countries served; a parent (an ISO country code like "es", or a geo_id like "R344953") lists what is under it.

⚠️ level means two different things:
- under a COUNTRY it selects that whole level, wherever it hangs (parent "es", level "city" = every Spanish city);
- under a PLACE it filters that place's direct children.

limit is at most %d, page at most %d. An empty list is a normal answer (a page past the end, a level this branch does not have), not an error.`, api.GeoMaxLimit, api.GeoMaxPage),
	}, s.browsePlaces)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "place_at_point",
		Annotations: readOnly("Name the places at a point"),
		Description: `FREE — costs no quota. Turn a coordinate into the H3 cell under it and the named zones that contain it, fine to coarse, each with its geo_id when it has one.

res is the H3 resolution of the cell (4–8; the server picks one when unset). Some zones come back with no geo_id and unknown ancestry: the API can name them but holds no hierarchy row — use the cell or another zone instead. No zones at all is a normal answer: the point is outside the geography this API holds.`,
	}, s.placeAtPoint)
}

// ─────────────────────────────────────────────────────────────────────────────
// find_place
// ─────────────────────────────────────────────────────────────────────────────

type findPlaceArgs struct {
	Q       string `json:"q" jsonschema:"the place name to resolve, e.g. Valencia or Ruzafa"`
	Country string `json:"country,omitempty" jsonschema:"ISO 3166-1 alpha-2 country code to search in, e.g. es"`
	Level   string `json:"level,omitempty" jsonschema:"restrict to one level: region, province, city, macrozone, microzone"`
	Limit   *int   `json:"limit,omitempty" jsonschema:"maximum hits; default 10, the server clamps it at 50 silently"`
}

func (s *server) findPlace(ctx context.Context, _ *mcp.CallToolRequest, args findPlaceArgs) (*mcp.CallToolResult, any, error) {
	c, err := s.client()
	if err != nil {
		return fail(err)
	}
	resp, err := c.GeoSearch(ctx, api.SearchParams{
		Q: args.Q, Country: args.Country, Level: args.Level, Limit: intOr0(args.Limit),
	})
	if err != nil {
		return fail(err)
	}
	return ok(resp, searchText(args.Q, resp), resp.Meta)
}

func searchText(query string, resp *api.SearchResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d hit(s) for %q.\n", len(resp.Results), query)
	if len(resp.Results) == 0 {
		b.WriteString("Nothing matched. That is an answer, not a failure: try a shorter name, drop country or level, or walk to it with browse_places.\n")
		return b.String()
	}
	b.WriteString("Same-named places are told apart by the chain under each hit, never by the name.\n")
	for i, hit := range resp.Results {
		fmt.Fprintf(&b, "\n%d. %s — %s, %s\n", i+1, orDash(hit.Name), orDash(hit.Level), orDash(hit.Country))
		fmt.Fprintf(&b, "   geo_id: %s\n", orDash(hit.GeoID))
		fmt.Fprintf(&b, "   in: %s\n", ancestryText(hit.Ancestors))
		if hit.IsPoint() {
			// The tree missed and the geocoder answered: a coordinate, not a
			// place in the hierarchy.
			fmt.Fprintf(&b, "   point: %s, %s (geocoder fallback; source %s)\n",
				floatText(hit.Lat), floatText(hit.Lng), orDash(hit.Source))
		}
		fmt.Fprintf(&b, "   %s\n", signalText(hit.PricesAvailable, hit.HasData, hit.CurrentPeriod))
	}
	return b.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// browse_places
// ─────────────────────────────────────────────────────────────────────────────

type browsePlacesArgs struct {
	Parent string `json:"parent,omitempty" jsonschema:"ISO country code (es) or a geo_id (R344953); empty lists the countries served"`
	Level  string `json:"level,omitempty" jsonschema:"under a country: select that whole level; under a place: filter its direct children. region, province, city, macrozone, microzone"`
	Limit  *int   `json:"limit,omitempty" jsonschema:"rows per page, at most 100"`
	Page   *int   `json:"page,omitempty" jsonschema:"page number from 1, at most 10000"`
}

func (s *server) browsePlaces(ctx context.Context, _ *mcp.CallToolRequest, args browsePlacesArgs) (*mcp.CallToolResult, any, error) {
	c, err := s.client()
	if err != nil {
		return fail(err)
	}
	// The caps are checked by GeoBrowse itself, before any request: one
	// rule, shared with the CLI.
	resp, err := c.GeoBrowse(ctx, api.GeoParams{
		Parent: strings.TrimSpace(args.Parent), Level: args.Level,
		Limit: intOr0(args.Limit), Page: intOr0(args.Page),
	})
	if err != nil {
		return fail(err)
	}
	return ok(resp, browseText(args, resp), resp.Meta)
}

func browseText(args browsePlacesArgs, resp *api.GeoResponse) string {
	var b strings.Builder
	switch {
	case resp.Parent != nil:
		fmt.Fprintf(&b, "Under %s (%s, geo_id %s)", orDash(resp.Parent.Name), orDash(resp.Parent.Level), orDash(resp.Parent.GeoID))
		if args.Level != "" {
			// Name which of level's two meanings applied, in the answer and
			// not only in the tool description.
			if resp.Parent.IsCountry() {
				fmt.Fprintf(&b, ", every %s in the country", args.Level)
			} else {
				fmt.Fprintf(&b, ", direct children at level %s", args.Level)
			}
		}
		b.WriteString(".\n")
		fmt.Fprintf(&b, "in: %s\n", ancestryText(resp.Parent.Ancestors))
	case strings.TrimSpace(args.Parent) == "":
		b.WriteString("The countries this API serves — the root of the tree.\n")
	default:
		fmt.Fprintf(&b, "Under %q.\n", args.Parent)
	}

	if len(resp.Results) == 0 {
		b.WriteString("No rows. That is a normal answer, not an error: the parent may not resolve, the page may be past the end, or this branch has no such level. Try find_place, or drop level.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%d row(s). Pass a geo_id as parent to go deeper, or as geo_id to market_stats.\n", len(resp.Results))
	for _, row := range resp.Results {
		fmt.Fprintf(&b, "- %s — %s, geo_id %s — %s\n", orDash(row.Name), orDash(row.Level), orDash(row.GeoID),
			signalText(row.PricesAvailable, row.HasData, row.CurrentPeriod))
	}
	return b.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// place_at_point
// ─────────────────────────────────────────────────────────────────────────────

// placeAtPointArgs takes lat and lng as plain numbers, both required: 0 is a
// real coordinate (the equator, Greenwich), so "not set" is the schema's job,
// not a nil pointer's.
type placeAtPointArgs struct {
	Lat float64 `json:"lat" jsonschema:"latitude in degrees, -90 to 90"`
	Lng float64 `json:"lng" jsonschema:"longitude in degrees, -180 to 180"`
	Res *int    `json:"res,omitempty" jsonschema:"H3 resolution of the cell, 4 to 8"`
}

func (s *server) placeAtPoint(ctx context.Context, _ *mcp.CallToolRequest, args placeAtPointArgs) (*mcp.CallToolResult, any, error) {
	// ⚠️ CHECKED HERE, BEFORE THE CLIENT IS EVEN BUILT. internal/api does not
	// range-check coordinates (the CLI's flags never produce one out of range
	// by accident), but an agent swapping lat and lng is a common mistake, and
	// lat 139 must not cost a round trip to learn it is wrong.
	switch {
	case args.Lat < -90 || args.Lat > 90:
		return fail(&api.ValidationError{Parameter: "lat",
			Message: fmt.Sprintf("lat must be between -90 and 90, got %v — are lat and lng swapped?", args.Lat)})
	case args.Lng < -180 || args.Lng > 180:
		return fail(&api.ValidationError{Parameter: "lng",
			Message: fmt.Sprintf("lng must be between -180 and 180, got %v", args.Lng)})
	}

	c, err := s.client()
	if err != nil {
		return fail(err)
	}
	point, err := c.GeoLookup(ctx, api.LookupParams{Lat: &args.Lat, Lng: &args.Lng, Res: args.Res})
	if err != nil {
		return fail(err)
	}
	return ok(point, pointText(point), point.Meta)
}

func pointText(point *api.Point) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Cell %s (res %d) at %s, %s.\n", orDash(point.H3), point.H3Res,
		floatText(&point.Lat), floatText(&point.Lng))
	fmt.Fprintf(&b, "%s (%s)\n", strText(point.DisplayName), strText(point.Country))
	b.WriteString(pointSignalText(point))
	b.WriteString("\n")

	if len(point.Zones) == 0 {
		b.WriteString("No zone contains this point. That is a normal answer: it is outside the named geography this API holds.\n")
		return b.String()
	}
	b.WriteString("Containing zones, fine to coarse:\n")
	for _, zone := range point.Zones {
		geoID := "none — not addressable"
		if zone.Addressable() {
			geoID = *zone.GeoID
		}
		fmt.Fprintf(&b, "- %s — %s, geo_id %s, data this period: %s\n  in: %s\n",
			orDash(zone.Name), orDash(zone.Level), geoID, dataNowText(zone.HasData, zone.CurrentPeriod),
			ancestryText(zone.Ancestors))
	}
	return b.String()
}

// pointSignalText names a move that exists for THIS answer. The cell is
// always spendable as h3; a zone's geo_id only when some zone carries one —
// this is the one endpoint whose zones can be unaddressable.
func pointSignalText(point *api.Point) string {
	if !point.PricesAvailable {
		return "availability: dead_end — no prices here, do not spend a metered call"
	}
	spend := fmt.Sprintf("availability: prices — spend the cell as market_stats h3 [%q]", point.H3)
	for _, zone := range point.Zones {
		if zone.Addressable() {
			return spend + ", or a zone's geo_id below"
		}
	}
	return spend + " (no zone here carries a geo_id)"
}

// ─────────────────────────────────────────────────────────────────────────────
// words
// ─────────────────────────────────────────────────────────────────────────────

// signalText is the three-state signal in the tools' words: the machine form
// first (stable, the same one structured readers would use), then what to do.
//
// The decision is api.PlaceAvailability — the CLI reads the same one, so the
// two can word it differently and never disagree about what the flags mean.
func signalText(pricesAvailable bool, hasData *bool, period api.NullableDate) string {
	signal := api.PlaceAvailability(pricesAvailable, hasData, period)
	var why string
	switch signal {
	case api.SignalDead:
		why = "no prices — dead end, do not spend a metered call"
	case api.SignalAsk:
		why = "prices, data in the current period — market_stats will answer"
		if hasData == nil {
			why = "prices, current period " + period.Value() + " — market_stats will answer"
		}
	case api.SignalThin:
		why = "prices, but nothing in the current period — try price_history, not a dead end"
	default:
		// Neither key was sent: this endpoint does not publish the signal,
		// which says nothing about the place either way.
		why = "prices — this answer does not say whether the current period has data"
	}
	return fmt.Sprintf("availability: %s — %s", signal, why)
}

// dataNowText is the short form of the data-this-period half, for zone rows.
func dataNowText(hasData *bool, period api.NullableDate) string {
	switch api.DataThisPeriod(hasData, period) {
	case api.DataYes:
		if hasData == nil {
			return period.Value()
		}
		return "yes"
	case api.DataNo:
		return "no — try price_history"
	default:
		return "not stated"
	}
}

// ancestryText is the chain fine to coarse, WITH the ids: names repeat, and
// the id at every step is what tells two same-named hits apart.
//
// ⚠️ null and [] are different answers (see api.Ancestors): "unknown" means
// try another selector, "top of the tree" means stop walking up.
func ancestryText(a api.Ancestors) string {
	switch {
	case !a.Known():
		return "ancestry unknown — the API holds no hierarchy row for this zone; try another selector"
	case a.Root():
		return "top of the tree — no ancestors"
	}
	parts := make([]string, 0, a.Len())
	for _, step := range a.List() {
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", step.Name, step.Level, step.GeoID))
	}
	return strings.Join(parts, " › ")
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

func strText(v *string) string {
	if v == nil {
		return "—"
	}
	return orDash(*v)
}
