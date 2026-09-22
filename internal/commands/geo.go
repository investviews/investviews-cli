package commands

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/investviews/investviews-cli/internal/api"
)

// levelHelp is the flag help for --level on browse. It states BOTH meanings,
// because the flag has two and the wrong one gives a confidently wrong answer.
const levelHelp = `one level; it means TWO different things, depending on --parent:
  --parent es --level city       every city in SPAIN — a whole-level jump, from any region
  --parent R349055 --level city  only THAT region's own cities — a filter on its direct children
under a COUNTRY --level names a whole level of the country; under a PLACE it filters that place's direct children`

// NewGeoCmd builds "investviews geo": the four free ways into the geography.
func NewGeoCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "geo",
		Short: "Walk, search and resolve the geography (free, uncounted)",
		Long: `Walk, search and resolve the geography this API knows.

All four subcommands are FREE and uncounted: discovery costs nothing, so look a
place up rather than guessing an id. Only "stats current" and "stats history"
spend quota.

Every row prints the geo_id the next call takes, so you reach an addressable
place spending only ids a previous call returned — never a guessed name.`,
	}
	cmd.AddCommand(
		newGeoBrowseCmd(deps),
		newGeoSearchCmd(deps),
		newGeoLookupCmd(deps),
		newGeoHexesCmd(deps),
	)
	return cmd
}

// ─────────────────────────────────────────────────────────────────────────────
// geo browse
// ─────────────────────────────────────────────────────────────────────────────

func newGeoBrowseCmd(deps Deps) *cobra.Command {
	var (
		parent string
		level  string
		limit  int
		page   int
		all    bool
	)

	cmd := &cobra.Command{
		Use:   "browse",
		Short: "Walk the geography from countries down, free",
		Long: `Walk the geography from the countries down.

This is the command to reach for when you have NO name to search. With no
argument it lists the countries; --parent takes any geo_id a previous call
printed, so you descend spending only ids the API gave you.

  investviews geo browse                           # the countries
  investviews geo browse --parent es               # Spain's top level
  investviews geo browse --parent es --level city  # EVERY city in Spain
  investviews geo browse --parent R349055          # that region's direct children

⚠️ --level MEANS TWO DIFFERENT THINGS, and which one depends on --parent.
Under a COUNTRY it names a whole level of that country — "--parent es --level
city" is every Spanish city, wherever it hangs in the tree, not the handful
sitting at the top of Spain's. Under a PLACE it filters that place's direct
children. The header line of every result says which of the two happened, so
the two moves are never confused after the fact.

⚠️ AN EMPTY RESULT IS OFTEN THE CORRECT ANSWER. Levels are SKIPPED, not
shifted: a city's direct children are macrozones, so "--level microzone" under
a city legitimately returns nothing, and a city whose province is blank hangs
straight off its region. "No rows at this level" is a normal answer and exits
0 — it never means the place has nothing below it.

Each row also carries the three-state availability signal, which decides
whether spending a metered stats call on it can pay off at all.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
	}
	v := addJSONFlag(cmd)
	cmd.Flags().StringVar(&parent, "parent", "",
		"a geo_id to descend from: a country code (es) or a place ref (R344953); omit for the country list")
	cmd.Flags().StringVar(&level, "level", "", levelHelp)
	cmd.Flags().IntVar(&limit, "limit", 0,
		fmt.Sprintf("page size, at most %d (server default %d)", api.GeoMaxLimit, api.GeoDefaultLimit))
	cmd.Flags().IntVar(&page, "page", 0, "1-based page; a page past the end is empty, not an error")
	cmd.Flags().BoolVar(&all, "all", false,
		"read every page. /geo publishes no total, so this reads until a short or empty page comes back")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		client, err := deps.client()
		if err != nil {
			return err
		}
		params := api.GeoParams{Parent: parent, Level: level, Limit: limit, Page: page}

		if all {
			var (
				rows      []api.GeoResult
				parentRow *api.GeoResult
				meta      api.Meta
				pages     int
			)
			err := client.GeoBrowseAll(cmd.Context(), params, func(p *api.GeoResponse) error {
				pages++
				if parentRow == nil {
					parentRow = p.Parent
				}
				rows = append(rows, p.Results...)
				meta = p.Meta
				return nil
			})
			if err != nil {
				return err
			}
			if rows == nil {
				rows = []api.GeoResult{}
			}
			resp := &api.GeoResponse{Parent: parentRow, Results: rows}
			return v.emit(cmd, resp, meta, pages, api.EndpointGeo, func(w io.Writer) {
				renderBrowse(w, parent, level, resp)
			})
		}

		resp, err := client.GeoBrowse(cmd.Context(), params)
		if err != nil {
			return err
		}
		return v.emit(cmd, resp, resp.Meta, 1, api.EndpointGeo, func(w io.Writer) {
			renderBrowse(w, parent, level, resp)
		})
	}
	return cmd
}

// renderBrowse prints the step that was taken, then the rows.
//
// The header is written from the response's own parent object where there is
// one: what the API resolved beats what was typed.
func renderBrowse(w io.Writer, parentFlag, level string, resp *api.GeoResponse) {
	fmt.Fprintln(w, browseHeader(parentFlag, level, resp.Parent))
	if resp.Parent != nil {
		fmt.Fprintf(w, "parent: %s (%s, %s)\n", resp.Parent.Name, dash(resp.Parent.Level), resp.Parent.GeoID)
		fmt.Fprintf(w, "  in: %s\n", ancestry(resp.Parent.Ancestors))
	}
	fmt.Fprintln(w)

	if len(resp.Results) == 0 {
		renderEmptyBrowse(w, parentFlag, level, resp.Parent)
		return
	}

	fmt.Fprintf(w, "%d result(s). Spend a GEO_ID below as --parent to go deeper, or as --geo-id on stats.\n\n",
		len(resp.Results))
	table(w, func(tw io.Writer) {
		fmt.Fprintln(tw, "GEO_ID\tLEVEL\tNAME\tAVAILABILITY")
		for _, row := range resp.Results {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				row.GeoID, dash(row.Level), row.Name, availability(row.PricesAvailable, row.HasData, row.CurrentPeriod))
		}
	})
}

// browseHeader names which of --level's two meanings was used, in the output
// and not only in the help text.
func browseHeader(parentFlag, level string, parent *api.GeoResult) string {
	isCountry := looksLikeCountryCode(parentFlag)
	name := strings.TrimSpace(parentFlag)
	if parent != nil {
		isCountry = parent.IsCountry()
		if parent.Name != "" {
			name = parent.Name
		}
	}

	switch {
	case strings.TrimSpace(parentFlag) == "" && level == "":
		return "The countries this API serves — the root of the cascade."
	case strings.TrimSpace(parentFlag) == "":
		return fmt.Sprintf("Every %s the API will list with no parent named (level filter: %s).", level, level)
	case isCountry && level != "":
		return fmt.Sprintf(
			"Every %s in %s — a WHOLE-LEVEL jump: these come from anywhere in the country, not only from the top of its tree.",
			level, name)
	case isCountry:
		return fmt.Sprintf("The top level of %s — its direct children.", name)
	case level != "":
		return fmt.Sprintf("Direct children of %s, filtered to level %s.", name, level)
	default:
		return fmt.Sprintf("Direct children of %s.", name)
	}
}

// renderEmptyBrowse is the normal answer for an empty listing. It is not an
// error, it does not exit non-zero, and it always names the next move.
//
// ⚠️ The next move is spelled with the id THIS call used, never as a
// placeholder. The caller already holds that id — it is what it passed as
// --parent — so a hint reading "<its geo_id>" asks it to go and find something
// it is standing on, and a hint is only worth printing if it can be run.
func renderEmptyBrowse(w io.Writer, parentFlag, level string, parent *api.GeoResult) {
	name := strings.TrimSpace(parentFlag)
	id := strings.TrimSpace(parentFlag)
	if parent != nil {
		if parent.Name != "" {
			name = parent.Name
		}
		if parent.GeoID != "" {
			id = parent.GeoID // what the API resolved beats what was typed
		}
	}
	if name == "" {
		name = "this listing"
	}
	if id == "" {
		id = "<its geo_id>"
	}

	if level != "" {
		fmt.Fprintf(w, "No rows at level %q under %s. That is a NORMAL answer, not an error.\n", level, name)
		fmt.Fprintln(w, "Levels are SKIPPED, not shifted: a city's direct children are macrozones, so")
		fmt.Fprintln(w, "asking a city for microzones legitimately returns nothing, and a city whose")
		fmt.Fprintln(w, "province is blank hangs straight off its region.")
		fmt.Fprintf(w, "Next: drop --level — `investviews geo browse --parent %s` — to see what %s\n", id, name)
		fmt.Fprintln(w, "actually has below it.")
		return
	}
	fmt.Fprintf(w, "%s has no children in the tree — the walk ends here. That is a NORMAL answer,\n", name)
	fmt.Fprintln(w, "not an error: fill drops down the tree, and most branches terminate above")
	fmt.Fprintln(w, "neighbourhood level.")
	fmt.Fprintf(w, "Next: query this place itself with `investviews stats current --geo-id %s`.\n", id)
}

// ─────────────────────────────────────────────────────────────────────────────
// geo search
// ─────────────────────────────────────────────────────────────────────────────

func newGeoSearchCmd(deps Deps) *cobra.Command {
	var (
		country string
		level   string
		limit   int
		all     bool
	)

	cmd := &cobra.Command{
		Use:   "search <name>",
		Short: "Resolve a place name to a geo_id, free",
		Long: `Resolve a place name into a geo_id you can spend.

  investviews geo search "ruzafa valencia"
  investviews geo search centro --country es --level macrozone

⚠️ NAMES ARE NOT UNIQUE. Expect several hits identical on name, level and
country — 12,233 of 89,965 microzone names spanned two or more parent cities
when that was last measured, in 2026-08 and before KAN-200; it has not been
re-measured since. Every hit therefore prints its ancestor chain WITH an id at
each step, which is the only thing that tells two same-named places apart and
the only form an agent can act on.

A miss is a 404 that may carry suggestions; they are printed when the API
sends any.

⚠️ There is no paging here at all: --limit is the only control and the server
clamps it silently. Narrow with --country or --level instead.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
	}
	v := addJSONFlag(cmd)
	cmd.Flags().StringVar(&country, "country", "", "restrict to one ISO 3166-1 alpha-2 code (es)")
	cmd.Flags().StringVar(&level, "level", "", "restrict to one level: region, province, city, macrozone, microzone")
	cmd.Flags().IntVar(&limit, "limit", 0,
		fmt.Sprintf("maximum hits (server default %d, silently clamped at %d)", api.SearchDefaultLimit, api.SearchServerClamp))
	cmd.Flags().BoolVar(&all, "all", false,
		"refused: /geo/search does not page. The flag exists so that asking fails loudly instead of appearing to work")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		client, err := deps.client()
		if err != nil {
			return err
		}
		if all {
			// ⚠️ Not silently ignored. A --all that printed one page under
			// an "everything" banner would tell the user they had seen it all.
			return client.GeoSearchAll(cmd.Context(), api.SearchParams{}, nil)
		}

		resp, err := client.GeoSearch(cmd.Context(), api.SearchParams{
			Q: args[0], Country: country, Level: level, Limit: limit,
		})
		if err != nil {
			// A miss carries the next move. Print it, then return the error
			// so the exit code still says the search failed.
			var unknown *api.UnknownPlaceError
			if errors.As(err, &unknown) {
				renderSearchMiss(cmd.ErrOrStderr(), args[0], unknown)
			}
			return err
		}
		return v.emit(cmd, resp, resp.Meta, 1, api.EndpointGeoSearch, func(w io.Writer) {
			renderSearch(w, args[0], resp)
		})
	}
	return cmd
}

func renderSearch(w io.Writer, query string, resp *api.SearchResponse) {
	fmt.Fprintf(w, "%d hit(s) for %q.\n", len(resp.Results), query)
	if len(resp.Results) == 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Nothing matched. That is an answer, not a failure: try a shorter name, drop")
		fmt.Fprintln(w, "--country or --level, or walk to it with `investviews geo browse`.")
		return
	}
	fmt.Fprintln(w, "Same-named places are told apart by the chain under each hit, never by the name.")
	fmt.Fprintln(w)

	for i, hit := range resp.Results {
		fmt.Fprintf(w, "%d. %s — %s, %s\n", i+1, hit.Name, dash(hit.Level), dash(hit.Country))
		fmt.Fprintf(w, "   geo_id: %s\n", dash(hit.GeoID))
		fmt.Fprintf(w, "   in:     %s\n", ancestry(hit.Ancestors))
		if hit.IsPoint() {
			// The tree missed and the geocoder answered: a coordinate, not
			// a place in the hierarchy.
			fmt.Fprintf(w, "   point:  %s, %s (geocoder fallback, source %s)\n",
				num(hit.Lat), num(hit.Lng), dash(hit.Source))
		}
		fmt.Fprintf(w, "   %s\n", availability(hit.PricesAvailable, hit.HasData, hit.CurrentPeriod))
	}
}

// renderSearchMiss prints the suggestions a miss carries.
//
// ⚠️ Each suggestion is an OBJECT with a geo_id, not a name, so it is rendered
// like a search hit rather than joined into a list. A name on its own would
// send the user straight back into the search that just missed; the id can be
// spent on the next call.
func renderSearchMiss(w io.Writer, query string, err *api.UnknownPlaceError) {
	fmt.Fprintf(w, "No place matched %q.\n", query)
	if len(err.DidYouMean) == 0 {
		fmt.Fprintln(w, "The API sent no suggestions. Walk to the place with `investviews geo browse`,")
		fmt.Fprintln(w, "or check the market is served at all with `investviews coverage`.")
		return
	}

	fmt.Fprintf(w, "\nDid you mean one of these %d? Each carries a geo_id you can spend straight away.\n\n",
		len(err.DidYouMean))
	for i, hit := range err.DidYouMean {
		fmt.Fprintf(w, "%d. %s — %s, %s\n", i+1, dash(hit.Name), dash(hit.Level), dash(hit.Country))
		fmt.Fprintf(w, "   geo_id: %s\n", dash(hit.GeoID))
		fmt.Fprintf(w, "   in:     %s\n", ancestry(hit.Ancestors))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Ask about one with `investviews stats current --geo-id <id>`, or walk into it")
	fmt.Fprintln(w, "with `investviews geo browse --parent <id>`.")
}

// ─────────────────────────────────────────────────────────────────────────────
// geo lookup
// ─────────────────────────────────────────────────────────────────────────────

func newGeoLookupCmd(deps Deps) *cobra.Command {
	var (
		hex string
		lat float64
		lng float64
		res int
	)

	cmd := &cobra.Command{
		Use:   "lookup",
		Short: "Name the zones containing one cell or one point, free",
		Long: `Turn a coordinate or an H3 cell into the places that contain it.

  investviews geo lookup --h3 613498079267520511
  investviews geo lookup --lat 39.4699 --lng -0.3763 --res 8

Name ONE of the two: a cell, or a point. A cell already states its own
resolution, so --res applies to the point form only.

⚠️ This is the one endpoint whose zones can come back with an UNKNOWN ancestry
— the API can name the zone but holds no hierarchy row for it. That prints as
"ancestry unknown", which is not the same answer as "top of the tree", and
those zones carry no geo_id to spend.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
	}
	v := addJSONFlag(cmd)
	cmd.Flags().StringVar(&hex, "h3", "",
		"one H3 cell: a decimal id (613498079267520511) or a canonical H3 string")
	cmd.Flags().Float64Var(&lat, "lat", 0, "latitude; requires --lng")
	cmd.Flags().Float64Var(&lng, "lng", 0, "longitude; requires --lat")
	cmd.Flags().IntVar(&res, "res", 0, "H3 resolution 4..8 for the --lat/--lng form; ignored with --h3")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		client, err := deps.client()
		if err != nil {
			return err
		}
		params := api.LookupParams{H3: hex}
		if cmd.Flags().Changed("lat") {
			params.Lat = &lat
		}
		if cmd.Flags().Changed("lng") {
			params.Lng = &lng
		}
		if cmd.Flags().Changed("res") {
			params.Res = &res
		}

		point, err := client.GeoLookup(cmd.Context(), params)
		if err != nil {
			return err
		}
		return v.emit(cmd, point, point.Meta, 1, api.EndpointGeoLookup, func(w io.Writer) {
			renderPoint(w, point)
		})
	}
	return cmd
}

func renderPoint(w io.Writer, point *api.Point) {
	fmt.Fprintf(w, "Cell %s (res %d) at %s, %s — source %s\n",
		dash(point.H3), point.H3Res, num(&point.Lat), num(&point.Lng), dash(point.Source))
	fmt.Fprintf(w, "%s (%s)\n", str(point.DisplayName), str(point.Country))
	fmt.Fprintf(w, "%s\n", availabilityForPoint(point))
	fmt.Fprintln(w)

	if len(point.Zones) == 0 {
		fmt.Fprintln(w, "No zone contains this cell. That is a normal answer — the cell is outside")
		fmt.Fprintln(w, "the named geography this API holds.")
		return
	}
	fmt.Fprintln(w, "Containing zones, fine to coarse:")
	table(w, func(tw io.Writer) {
		fmt.Fprintln(tw, "LEVEL\tNAME\tGEO_ID\tDATA THIS PERIOD\tANCESTRY")
		for _, zone := range point.Zones {
			geoID := "—"
			if zone.Addressable() {
				geoID = *zone.GeoID
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				dash(zone.Level), zone.Name, geoID, dataNow(zone.HasData, zone.CurrentPeriod), ancestry(zone.Ancestors))
		}
	})
}

// availabilityForPoint names a move that exists for THIS answer.
//
// ⚠️ It cannot offer "spend a zone's geo_id" unconditionally: a cell can come
// back with no zones at all, or with zones that carry no geo_id (this is the
// one endpoint whose zones can be unaddressable), and the line is printed
// ABOVE the zone table, so it would be promising an id the reader is then told
// does not exist. The cell's own id is always there and is always spendable.
func availabilityForPoint(point *api.Point) string {
	if !point.PricesAvailable {
		return "no prices here — dead end, do not spend a metered call"
	}
	spend := fmt.Sprintf("prices available — spend this cell: `investviews stats current --h3 %s`", point.H3)
	for _, zone := range point.Zones {
		if zone.Addressable() {
			return spend + ", or an addressable zone's geo_id below"
		}
	}
	// Either no zones, or none of them carries a geo_id.
	return spend + " (no zone here carries a geo_id to spend)"
}

// ─────────────────────────────────────────────────────────────────────────────
// geo hexes
// ─────────────────────────────────────────────────────────────────────────────

func newGeoHexesCmd(deps Deps) *cobra.Command {
	var (
		geoID   string
		cursor  string
		limit   int
		all     bool
		idsOnly bool
	)

	cmd := &cobra.Command{
		Use:   "hexes [geo_id]",
		Short: "List a place's H3 cells, free",
		Long: `List the H3 cells that make up a place. The ids come back ready to feed
straight into "stats current --h3".

  investviews geo hexes R344953
  investviews geo hexes --geo-id R344953 --all
  investviews geo hexes R344953 --all --ids-only | investviews stats current --h3 -

⚠️ PIPE WITH --ids-only, NEVER WITHOUT IT. The default output is written for a
reader: a header line, an availability sentence, a "more cells" hint and the
cost line surround the ids. Piped into "stats current --h3 -" those words are
split up and sent as cell ids to a METERED endpoint. --ids-only prints the ids
and nothing else, and puts the cost line on stderr so stdout stays a clean
stream. ("stats current" also refuses anything that is not shaped like a cell
id locally now, so the mistake is caught either way — but --ids-only is what
makes the pipe correct rather than merely refused.)

⚠️ Cell ids are DECIMAL int64 strings (613498076398616575), not 87… hex
strings.

⚠️ This endpoint pages by CURSOR, and a cursor is bound to the exact query
that issued it: one place's cursor used against another is refused with
invalid_cursor, and retrying it never succeeds — restart the listing with no
--cursor. --all walks the cursor for you and refuses to start from one.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
	}
	v := addJSONFlag(cmd)
	cmd.Flags().StringVar(&geoID, "geo-id", "", "the place whose cells to list; may also be given as the argument")
	cmd.Flags().StringVar(&cursor, "cursor", "", "the previous page's next_cursor, verbatim")
	cmd.Flags().IntVar(&limit, "limit", 0,
		fmt.Sprintf("cells per page, at most %d (server default %d)", api.HexesMaxLimit, api.HexesDefaultLimit))
	cmd.Flags().BoolVar(&all, "all", false, "read every page, walking the cursor")
	cmd.Flags().BoolVar(&idsOnly, "ids-only", false,
		"print the cell ids and NOTHING else, one per line, for a pipe into `stats current --h3 -`; "+
			"the cost line moves to stderr. --json wins if both are given")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		client, err := deps.client()
		if err != nil {
			return err
		}
		id, err := oneGeoID(geoID, args)
		if err != nil {
			return err
		}
		params := api.HexesParams{Cursor: cursor, Limit: limit}
		v.bare = idsOnly

		if all {
			var (
				first *api.HexPage
				cells []string
				meta  api.Meta
				pages int
			)
			err := client.PlaceHexesAll(cmd.Context(), id, params, func(p *api.HexPage) error {
				pages++
				if first == nil {
					first = p
				}
				cells = append(cells, p.Hexes...)
				meta = p.Meta
				return nil
			})
			if err != nil {
				return err
			}
			page := *first
			page.Hexes = cells
			page.NextCursor = nil
			return v.emit(cmd, &page, meta, pages, api.EndpointGeoHexes, func(w io.Writer) {
				if idsOnly {
					renderHexIDs(w, &page)
					return
				}
				renderHexes(w, &page, true)
			})
		}

		page, err := client.PlaceHexes(cmd.Context(), id, params)
		if err != nil {
			return err
		}
		if idsOnly && page.HasMore() {
			// ⚠️ A PARTIAL PIPE IS THE ONE FAILURE --ids-only CAN STILL
			// CAUSE, and it is silent: the receiving command gets a valid
			// list of cells that is only part of the place, and answers
			// confidently about the part. Say so on stderr, where it does
			// not contaminate the stream.
			fmt.Fprintf(cmd.ErrOrStderr(),
				"⚠️ partial: this is one page of %s's cells, not all of them. Add --all, or the "+
					"answer downstream describes part of the place.\n", page.GeoID)
		}
		return v.emit(cmd, page, page.Meta, 1, api.EndpointGeoHexes, func(w io.Writer) {
			if idsOnly {
				renderHexIDs(w, page)
				return
			}
			renderHexes(w, page, false)
		})
	}
	return cmd
}

// renderHexIDs prints the cells and nothing at all besides — no header, no
// availability line, no cursor hint. That is the whole point: it is the shape
// `stats current --h3 -` reads, and every extra word in it would arrive there
// as a cell id.
func renderHexIDs(w io.Writer, page *api.HexPage) {
	for _, cell := range page.Hexes {
		fmt.Fprintln(w, cell)
	}
}

// oneGeoID takes the id from the flag or the argument, and refuses both.
func oneGeoID(flag string, args []string) (string, error) {
	flag = strings.TrimSpace(flag)
	var arg string
	if len(args) == 1 {
		arg = strings.TrimSpace(args[0])
	}
	switch {
	case flag != "" && arg != "":
		return "", &api.ValidationError{
			Parameter: "geo_id",
			Message:   "the geo_id was given twice, as --geo-id and as the argument — give it once",
		}
	case flag != "":
		return flag, nil
	case arg != "":
		return arg, nil
	default:
		return "", &api.ValidationError{
			Parameter: "geo_id",
			Message: "a geo_id is required; resolve one with `investviews geo search` or " +
				"`investviews geo browse`",
		}
	}
}

func renderHexes(w io.Writer, page *api.HexPage, complete bool) {
	scope := "this page"
	if complete {
		scope = "every page"
	}
	fmt.Fprintf(w, "%s %s (%s, %s) — %d cell(s) at res %d, %s\n",
		dash(page.GeoID), page.Name, dash(page.Level), dash(page.Country), len(page.Hexes), page.H3Res, scope)
	fmt.Fprintf(w, "%s\n", availability(page.PricesAvailable, nil, api.NullableDate{}))
	fmt.Fprintln(w)

	if len(page.Hexes) == 0 {
		fmt.Fprintln(w, "No cells. That is a normal answer — this place holds no cell at a resolution")
		fmt.Fprintln(w, "the API publishes.")
		return
	}
	for _, cell := range page.Hexes {
		fmt.Fprintln(w, cell)
	}
	if page.HasMore() {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "more cells: --cursor %s (or --all to read every page)\n", *page.NextCursor)
		fmt.Fprintln(w, "⚠️ that cursor belongs to THIS query; do not use it against another place.")
	}
}
