package api

import (
	"context"
	"fmt"
)

// ⚠️ THREE PAGING MODELS. ONE --all CANNOT SERVE THEM.
//
//	endpoint                model    limits
//	────────────────────────────────────────────────────────────────────────
//	/geo                    offset   limit ≤ 100 (default 50), page ≤ 10000
//	/geo/search             none     limit only, default 10, SERVER CLAMPS 50
//	/geo/{geo_id}/hexes     cursor   limit ≤ 500 (default 500), keyset cursor
//
// Each endpoint gets its own walker below. There is deliberately no generic
// one: the three differ in how they end (a short page, never, a null cursor),
// and a walker that guessed would silently stop early on one of them.

// PageMode names how an endpoint pages.
type PageMode string

const (
	// PageOffset is limit + a 1-based page number.
	//
	// ⚠️ The /geo response carries NO paging metadata — no total, no
	// has_more, no page echo — so the only end signal is a page that comes
	// back shorter than the limit, or empty. The total is unknowable in
	// advance.
	PageOffset PageMode = "offset"

	// PageNone is one page and no more. --all is meaningless here and must
	// say so rather than appear to work.
	PageNone PageMode = "none"

	// PageCursor is opaque keyset paging.
	//
	// ⚠️ A cursor is scoped to the EXACT query that issued it. Valencia's
	// cursor against Barcelona is 400 invalid_cursor. Never carry one across
	// places, and never across a filter change.
	PageCursor PageMode = "cursor"
)

// Paging describes one endpoint's paging so a command can build its flags
// from it rather than from memory.
type Paging struct {
	Mode         PageMode
	DefaultLimit int
	// MaxLimit is the documented ceiling, 0 when none is documented.
	MaxLimit int
	// ServerClamp is a ceiling the server applies silently, without
	// refusing the request. 0 when there is none.
	ServerClamp int
	// MaxPage bounds offset paging.
	MaxPage int
	// Note is what to tell a user who asked for --all on this endpoint.
	Note string
}

// SupportsAll reports whether walking every page is possible here.
func (p Paging) SupportsAll() bool { return p.Mode != PageNone }

// Paging limits, measured against the live API 2026-09-12.
const (
	GeoDefaultLimit = 50
	GeoMaxLimit     = 100
	GeoMaxPage      = 10000

	SearchDefaultLimit = 10
	// SearchServerClamp is applied silently: limit=100 returns 50 rows,
	// limit=1000 returns 200 OK and 50 rows. The server clamps, never refuses.
	SearchServerClamp = 50

	HexesDefaultLimit = 500
	HexesMaxLimit     = 500
)

// LookupMinRes and LookupMaxRes bound the H3 resolution /geo/lookup takes for
// a coordinate.
const (
	LookupMinRes = 4
	LookupMaxRes = 8
)

// GeoPaging describes /geo.
func GeoPaging() Paging {
	return Paging{
		Mode:         PageOffset,
		DefaultLimit: GeoDefaultLimit,
		MaxLimit:     GeoMaxLimit,
		MaxPage:      GeoMaxPage,
		Note: "pages by offset; the response carries no total, so --all reads pages " +
			"until a short or empty one comes back",
	}
}

// SearchPaging describes /geo/search.
func SearchPaging() Paging {
	return Paging{
		Mode:         PageNone,
		DefaultLimit: SearchDefaultLimit,
		ServerClamp:  SearchServerClamp,
		Note: fmt.Sprintf("does not page: it takes a limit and nothing else, and the server "+
			"clamps that at %d. Narrow the query with --country or --level to see other "+
			"results — there is no second page to ask for", SearchServerClamp),
	}
}

// HexesPaging describes /geo/{geo_id}/hexes.
func HexesPaging() Paging {
	return Paging{
		Mode:         PageCursor,
		DefaultLimit: HexesDefaultLimit,
		MaxLimit:     HexesMaxLimit,
		Note: "pages by cursor; the cursor is bound to this exact query, so --all reuses " +
			"one query and never carries a cursor across places",
	}
}

// GeoBrowseAll walks every page of /geo, calling fn once per page.
//
// Offset paging with no metadata: it stops on the first page shorter than the
// limit, on an empty page, or at MaxPage. fn returning an error stops the walk
// and that error is returned.
//
// ⚠️ Every page is a separate request. /geo is uncounted, so this costs no
// quota, but it does spend the per-minute rate budget.
func (c *Client) GeoBrowseAll(ctx context.Context, params GeoParams, fn func(*GeoResponse) error) error {
	if fn == nil {
		return fmt.Errorf("api: GeoBrowseAll needs a page handler")
	}
	limit := params.Limit
	if limit <= 0 {
		limit = GeoMaxLimit // fewest round trips when the caller did not choose
	}
	if limit > GeoMaxLimit {
		limit = GeoMaxLimit
	}

	page := params.Page
	if page <= 0 {
		page = 1
	}
	var previous []string

	for ; page <= GeoMaxPage; page++ {
		p := params
		p.Limit, p.Page = limit, page

		resp, err := c.GeoBrowse(ctx, p)
		if err != nil {
			return err
		}
		if len(resp.Results) == 0 {
			return nil
		}

		// The contract says a page past the end is empty rather than a
		// repeat of the last one. Check anyway: a server that ever did
		// repeat would spin here forever, and a silent break would look
		// exactly like the end of the listing.
		ids := geoIDs(resp.Results)
		if sameIDs(previous, ids) {
			return fmt.Errorf("%w: /geo returned the same %d results for page %d as for page %d",
				ErrPagingStalled, len(ids), page, page-1)
		}
		previous = ids

		if err := fn(resp); err != nil {
			return err
		}

		// ⚠️ ONLY A PAGE THAT IS EXACTLY THE LIMIT CAN HAVE A NEXT ONE.
		//
		// Shorter means the listing ended — that is the documented signal.
		// LONGER means the server ignored the limit, and an endpoint that
		// ignores the limit is not paging at all: there is no page 2, and
		// asking for one returns the same rows again. /geo does exactly
		// this at its root, where it answers with every country whatever
		// limit is asked for, but the rule is written as the rule rather
		// than as a root special case, so it also covers the next endpoint
		// that behaves this way.
		//
		// The stall guard above stays and is still needed: it catches a
		// server that repeats a FULL page. This only means the walker no
		// longer has to be caught by it — `--all --limit 10` at the root
		// used to exit 1 on a query that was perfectly well formed.
		if len(resp.Results) != limit {
			return nil
		}
	}
	return fmt.Errorf("api: /geo paging stopped at the page ceiling (%d); narrow the query with --level or a deeper --parent", GeoMaxPage)
}

// GeoBrowseCollect walks every page of /geo and returns every row.
func (c *Client) GeoBrowseCollect(ctx context.Context, params GeoParams) ([]GeoResult, Meta, error) {
	var all []GeoResult
	var last Meta
	err := c.GeoBrowseAll(ctx, params, func(page *GeoResponse) error {
		all = append(all, page.Results...)
		last = page.Meta
		return nil
	})
	return all, last, err
}

// GeoSearchAll exists so that --all on search FAILS LOUDLY instead of looking
// like it worked.
//
// /geo/search has no page and no cursor: one limit, silently clamped at 50 by
// the server. A walker here could only ever return the same single page, and a
// command that printed it under an --all banner would be telling the user they
// had seen everything.
func (c *Client) GeoSearchAll(_ context.Context, _ SearchParams, _ func(*SearchResponse) error) error {
	return fmt.Errorf("%w: %s", ErrNoPaging, SearchPaging().Note)
}

// PlaceHexesAll walks every page of a place's cells, calling fn once per page.
//
// ⚠️ It refuses a caller-supplied cursor. A cursor is bound to the query that
// issued it, so starting a full walk from someone else's position is either an
// invalid_cursor or, worse, a silently partial listing that looks complete.
func (c *Client) PlaceHexesAll(ctx context.Context, geoID string, params HexesParams, fn func(*HexPage) error) error {
	if fn == nil {
		return fmt.Errorf("api: PlaceHexesAll needs a page handler")
	}
	if params.Cursor != "" {
		return &ValidationError{
			Parameter: "cursor",
			Message: "a full cell listing starts from no cursor: a cursor is bound to the exact " +
				"query that issued it, so resuming one here would either be refused as " +
				"invalid_cursor or quietly return a partial listing",
		}
	}
	limit := params.Limit
	if limit <= 0 {
		limit = HexesMaxLimit
	}
	if limit > HexesMaxLimit {
		limit = HexesMaxLimit
	}

	p := params
	p.Limit = limit
	for {
		page, err := c.PlaceHexes(ctx, geoID, p)
		if err != nil {
			return err
		}
		if err := fn(page); err != nil {
			return err
		}
		if !page.HasMore() {
			return nil
		}
		// Pass the cursor back verbatim, with every other parameter
		// unchanged — that is what keeps it valid.
		p.Cursor = *page.NextCursor
	}
}

// PlaceHexesCollect walks every page and returns every cell id, ready to feed
// straight into /stats/current?h3=.
func (c *Client) PlaceHexesCollect(ctx context.Context, geoID string, params HexesParams) ([]string, Meta, error) {
	var all []string
	var last Meta
	err := c.PlaceHexesAll(ctx, geoID, params, func(page *HexPage) error {
		all = append(all, page.Hexes...)
		last = page.Meta
		return nil
	})
	return all, last, err
}

func geoIDs(results []GeoResult) []string {
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.GeoID
	}
	return ids
}

func sameIDs(a, b []string) bool {
	if a == nil || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
