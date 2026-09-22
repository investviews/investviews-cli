package api

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ─────────────────────────────────────────────────────────────────────────────
// Ancestors — the one field where JSON null and [] mean different things
// ─────────────────────────────────────────────────────────────────────────────

// Ancestor is one step up the tree, addressable by GeoID. The id is what makes
// an ancestor actionable: 13.6% of neighbourhood names repeat inside their own
// country, so the name alone cannot tell two results apart.
type Ancestor struct {
	GeoID string `json:"geo_id"`
	Name  string `json:"name"`
	Level string `json:"level"`
}

// Ancestors is a place's ancestry, fine to coarse, ending on its country.
//
// ⚠️ null and [] are DIFFERENT ANSWERS and this type exists to keep them apart:
//
//   - []   — the place genuinely has no ancestors (a country at the root).
//     Known() is true, Root() is true. An agent should STOP walking up.
//   - null — we hold no place for that zone and cannot say. Known() is false.
//     An agent should TRY A DIFFERENT SELECTOR, not conclude it is at the root.
//
// A plain []Ancestor collapses both to a nil slice and destroys the
// distinction, so this carries a presence flag beside the list.
//
// ⚠️ The asymmetry is the trap: /geo and /geo/search are NEVER null (the
// contract says "never null and never absent"). It is null ONLY inside a
// /geo/lookup point's zones[]. Code tested against search meets the null later,
// on the branch hardest to reach.
//
// ⚠️ It was a comma-separated STRING until 2026-09-10. A bare JSON string is a
// DECODE ERROR here, deliberately: typing this field as a string compiles,
// runs, and silently loses every id.
type Ancestors struct {
	present bool
	list    []Ancestor
}

// NewAncestors builds a present, known ancestry. A nil list is a present,
// empty one — the root answer, not the unknown one.
func NewAncestors(list []Ancestor) Ancestors {
	if list == nil {
		list = []Ancestor{}
	}
	return Ancestors{present: true, list: list}
}

// UnknownAncestors is the null answer: we cannot say.
func UnknownAncestors() Ancestors { return Ancestors{} }

// Known reports whether the API told us the ancestry at all. False means the
// value was null or the key was absent.
func (a Ancestors) Known() bool { return a.present }

// Root reports the genuinely-empty answer: this place has no ancestors.
// False for the unknown answer — check Known first.
func (a Ancestors) Root() bool { return a.present && len(a.list) == 0 }

// List is the ancestry, fine to coarse. Empty for both the root answer and
// the unknown one, which is exactly why Known exists.
func (a Ancestors) List() []Ancestor { return a.list }

// Len is the number of steps up the tree.
func (a Ancestors) Len() int { return len(a.list) }

// Country is the last entry, which the contract says is always the country.
func (a Ancestors) Country() (Ancestor, bool) {
	if len(a.list) == 0 {
		return Ancestor{}, false
	}
	return a.list[len(a.list)-1], true
}

func (a *Ancestors) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		a.present, a.list = false, nil
		return nil
	}
	var list []Ancestor
	if err := json.Unmarshal(data, &list); err != nil {
		// A bare string lands here. That is the 2026-09-10 shape and it
		// must fail loudly rather than decode to nothing.
		return fmt.Errorf("ancestors must be an array of {geo_id,name,level} objects or null: %w", err)
	}
	if list == nil {
		list = []Ancestor{}
	}
	a.present, a.list = true, list
	return nil
}

func (a Ancestors) MarshalJSON() ([]byte, error) {
	if !a.present {
		return []byte("null"), nil
	}
	if a.list == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(a.list)
}

// ─────────────────────────────────────────────────────────────────────────────
// NullableDate — current_period, where null and absent are also different
// ─────────────────────────────────────────────────────────────────────────────

// NullableDate is a date field that can be null or absent.
//
// current_period uses it, and there the three states are three answers:
//
//	Set && Valid  — the window /stats/current would answer this place from.
//	Set && !Valid — null: nothing in the CURRENT window. NOT "no data ever":
//	                /stats/history may still answer for older windows.
//	!Set          — the endpoint does not publish the field at all (a hex
//	                page, for one). Says nothing about the place.
//
// ⚠️ current_period is the API's OLD signal. Since 2026-09-20 the API sends
// has_data instead (see GeoResult.HasData), and the field is kept here only so
// this CLI still reads a server that has not been upgraded yet. A field tagged
// with it must carry `omitzero`: --json re-encodes these structs, and without
// it an ABSENT current_period comes back out as `null` — which reads as
// "nothing this period" on every row of a server that never said so.
type NullableDate struct {
	set   bool
	valid bool
	value string
}

// Date builds a present, non-null date.
func Date(value string) NullableDate { return NullableDate{set: true, valid: true, value: value} }

// NullDate builds the explicit null: present in the payload, no value.
func NullDate() NullableDate { return NullableDate{set: true} }

// Present reports whether the key was in the payload at all.
func (d NullableDate) Present() bool { return d.set }

// Valid reports whether there is a date. False for both null and absent.
func (d NullableDate) Valid() bool { return d.valid }

// IsNull reports the explicit null — present, and deliberately empty.
func (d NullableDate) IsNull() bool { return d.set && !d.valid }

// Value is the date, empty unless Valid.
func (d NullableDate) Value() string { return d.value }

// IsZero reports the absent state. encoding/json's `omitzero` reads it, which
// is what keeps an absent key absent when a response is re-encoded for --json.
func (d NullableDate) IsZero() bool { return !d.set }

func (d NullableDate) String() string {
	switch {
	case d.valid:
		return d.value
	case d.set:
		return "null"
	default:
		return ""
	}
}

func (d *NullableDate) UnmarshalJSON(data []byte) error {
	d.set = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		d.valid, d.value = false, ""
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("expected a date string or null: %w", err)
	}
	d.valid, d.value = true, s
	return nil
}

func (d NullableDate) MarshalJSON() ([]byte, error) {
	if !d.valid {
		return []byte("null"), nil
	}
	return json.Marshal(d.value)
}

// ─────────────────────────────────────────────────────────────────────────────
// Geography
// ─────────────────────────────────────────────────────────────────────────────

// GeoResult is one row of /geo: a country at the root, a zone below it. Kind
// discriminates; the contract's GeoCountry and Zone differ only in which of
// these fields they populate.
type GeoResult struct {
	Kind      string    `json:"kind"`
	GeoID     string    `json:"geo_id"`
	Name      string    `json:"name"`
	Level     string    `json:"level"`
	Country   string    `json:"country"`
	Ancestors Ancestors `json:"ancestors"`

	H3Res    *int   `json:"h3_res,omitempty"`
	HexesURL string `json:"hexes_url,omitempty"`

	PricesAvailable bool `json:"prices_available"`
	// HasData is the API's current signal: does this place hold enough for
	// /stats/current to answer in the newest built period. nil means the
	// server did not send it — a server older than 2026-09-20, which sends
	// CurrentPeriod instead. See DataThisPeriod.
	HasData          *bool        `json:"has_data,omitempty"`
	CurrentPeriod    NullableDate `json:"current_period,omitzero"`
	ReportsAvailable *bool        `json:"reports_available,omitempty"`
}

// IsCountry reports whether this row is a country in the cascade root, whose
// GeoID is the ISO code.
func (g GeoResult) IsCountry() bool { return g.Kind == "country" || g.Level == "country" }

// Queryable reports whether spending a metered /stats/current call on this row
// can pay off. prices_available false means STOP; no data this period means
// the current window holds nothing, though /stats/history may still answer.
func (g GeoResult) Queryable() bool {
	return g.PricesAvailable && DataThisPeriod(g.HasData, g.CurrentPeriod) == DataYes
}

// DataSignal is "does the newest built period hold data here", read from
// whichever field the server sent.
type DataSignal int

const (
	// DataUnknown — the server sent neither field (a hex page, for one).
	// Says nothing about the place either way.
	DataUnknown DataSignal = iota
	// DataYes — /stats/current is worth asking.
	DataYes
	// DataNo — too little THIS period. NOT "no data ever": /stats/history may
	// still answer, and the API calls this answer conservative, not proof.
	DataNo
)

// DataThisPeriod folds the two versions of the API's signal into one answer.
//
// has_data (a boolean) replaced current_period (a date) on 2026-09-20, and the
// two mean the same three things: true / a date = ask; false / null = too thin
// this period; absent = not published. has_data wins when both are present,
// because it is the newer contract.
func DataThisPeriod(hasData *bool, currentPeriod NullableDate) DataSignal {
	switch {
	case hasData != nil && *hasData:
		return DataYes
	case hasData != nil:
		return DataNo
	case currentPeriod.Valid():
		return DataYes
	case currentPeriod.IsNull():
		return DataNo
	default:
		return DataUnknown
	}
}

// GeoResponse is /geo. Parent is absent on the country list, so a caller can
// tell the root from a branch without keeping its own state.
//
// ⚠️ It carries NO paging metadata — no total, no has_more, no page echo. That
// is why offset paging has to run until a short or empty page arrives.
type GeoResponse struct {
	Parent  *GeoResult  `json:"parent,omitempty"`
	Results []GeoResult `json:"results"`

	Meta Meta `json:"-"`
}

// SearchResult is one row of /geo/search: a zone from the location tree, or a
// point when the tree missed and the geocoder answered. Kind discriminates.
type SearchResult struct {
	GeoResult

	Lat         *float64      `json:"lat,omitempty"`
	Lng         *float64      `json:"lng,omitempty"`
	H3          string        `json:"h3,omitempty"`
	Source      string        `json:"source,omitempty"`
	DisplayName *string       `json:"display_name,omitempty"`
	Zones       []ZoneSummary `json:"zones,omitempty"`
}

// IsPoint reports the geocoder-fallback shape.
func (s SearchResult) IsPoint() bool { return s.Kind == "point" }

// SearchResponse is /geo/search. Query is absent in coordinate mode.
//
// ⚠️ There is no paging here at all — see Paging and ErrNoPaging.
type SearchResponse struct {
	Query   string         `json:"query,omitempty"`
	Results []SearchResult `json:"results"`

	Meta Meta `json:"-"`
}

// ZoneSummary is a containing zone on a point result.
//
// ⚠️ This is the ONE place Ancestors comes back null: a zone we can name but
// hold no hierarchy row for. GeoID is nil there too, rather than an id that
// would 404.
type ZoneSummary struct {
	Level         string       `json:"level"`
	Name          string       `json:"name"`
	Ancestors     Ancestors    `json:"ancestors"`
	GeoID         *string      `json:"geo_id"`
	HasData       *bool        `json:"has_data,omitempty"`
	CurrentPeriod NullableDate `json:"current_period,omitzero"`
	HexesURL      *string      `json:"hexes_url"`
}

// Addressable reports whether this zone carries an id worth spending.
func (z ZoneSummary) Addressable() bool { return z.GeoID != nil && *z.GeoID != "" }

// Point is /geo/lookup's answer: a spot on the map and the zones containing
// it. Returned directly, not wrapped in results — there is one answer per cell.
type Point struct {
	Kind             string        `json:"kind"`
	Lat              float64       `json:"lat"`
	Lng              float64       `json:"lng"`
	H3               string        `json:"h3"`
	H3Res            int           `json:"h3_res"`
	Source           string        `json:"source"`
	Country          *string       `json:"country"`
	DisplayName      *string       `json:"display_name"`
	Zones            []ZoneSummary `json:"zones"`
	PricesAvailable  bool          `json:"prices_available"`
	ReportsAvailable bool          `json:"reports_available"`

	Meta Meta `json:"-"`
}

// HexPage is one page of a place's cells.
//
// ⚠️ NextCursor is opaque and bound to THIS query. Pass it back verbatim; nil
// means the listing is finished, and that is the only end-of-listing signal.
// A cursor from another place or another filter set is refused with
// 400 invalid_cursor.
type HexPage struct {
	GeoID            string   `json:"geo_id"`
	Name             string   `json:"name"`
	Level            string   `json:"level"`
	Country          string   `json:"country"`
	H3Res            int      `json:"h3_res"`
	Hexes            []string `json:"hexes"`
	NextCursor       *string  `json:"next_cursor"`
	PricesAvailable  bool     `json:"prices_available"`
	ReportsAvailable bool     `json:"reports_available"`

	Meta Meta `json:"-"`
}

// HasMore reports whether another page exists.
func (p HexPage) HasMore() bool { return p.NextCursor != nil && *p.NextCursor != "" }

// ─────────────────────────────────────────────────────────────────────────────
// Metadata
// ─────────────────────────────────────────────────────────────────────────────

// PingResponse is /ping. Unauthenticated, so it answers "is the service up"
// even when the token is wrong.
type PingResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	DocsURL string `json:"docs_url"`

	Meta Meta `json:"-"`
}

// CoverageCountry is one market.
type CoverageCountry struct {
	Country string `json:"country"`
	// H3Resolutions is the resolutions held for this market.
	H3Resolutions []int `json:"h3_resolutions"`
	// LastAdParsedAt is the staleness field. ⚠️ null means no source targets
	// this market directly, NOT that it is empty.
	LastAdParsedAt *string `json:"last_ad_parsed_at"`
	// Searchable says whether /geo/search can resolve anything here. A market
	// can hold listings and still name nothing — that gap is the useful part.
	Searchable       bool `json:"searchable"`
	PricesAvailable  bool `json:"prices_available"`
	ReportsAvailable bool `json:"reports_available"`
}

// CoverageResponse is /coverage, computed live per request.
type CoverageResponse struct {
	WindowDays int               `json:"window_days"`
	Freshness  string            `json:"freshness"`
	Coverage   string            `json:"coverage"`
	Countries  []CoverageCountry `json:"countries"`

	Meta Meta `json:"-"`
}

// FiltersResponse is /meta/filters. Filters stays raw: the per-filter shapes
// differ (value lists, bin edges, a log grid) and the contract says new
// filters appear here first, so a struct would go stale against it.
type FiltersResponse struct {
	Version        string                     `json:"version"`
	DocsURL        string                     `json:"docs_url"`
	RangeSemantics string                     `json:"range_semantics"`
	Filters        map[string]json.RawMessage `json:"filters"`

	Meta Meta `json:"-"`
}

// Values reads one filter's accepted values, for the filters that publish a
// plain list (ad_type, ad_sub_type, rooms, level, currency).
func (f FiltersResponse) Values(name string) ([]string, bool) {
	raw, ok := f.Filters[name]
	if !ok {
		return nil, false
	}
	var body struct {
		Values []string `json:"values"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Values == nil {
		return nil, false
	}
	return body.Values, true
}

// TokenReport is the calling token as its owner may safely be shown it.
type TokenReport struct {
	Name        string  `json:"name"`
	ReadOnly    bool    `json:"read_only"`
	MaskedToken string  `json:"masked_token"`
	Status      string  `json:"status"`
	CreatedAt   *string `json:"created_at"`
	LastUsedAt  *string `json:"last_used_at"`
}

// UsageReport is one endpoint group's standing.
//
// ⚠️ On an uncounted group Limit, Used, Remaining and ResetsAt are null — NOT
// zero, which would read as "no allowance left". Pointers keep that apart.
type UsageReport struct {
	Metered    bool    `json:"metered"`
	Limit      *int64  `json:"limit"`
	Used       *int64  `json:"used"`
	Remaining  *int64  `json:"remaining"`
	ResetsAt   *string `json:"resets_at"`
	RatePerMin int     `json:"rate_per_min"`
	Source     string  `json:"source"`
	// Degraded means the metering store was unreachable, so the figures are
	// the committed balance alone. The API fails open: requests are served.
	Degraded bool `json:"degraded"`
}

// UsageResponse is /usage. Groups carries every group, including the uncounted
// ones, so "free" is distinguishable from "missing".
type UsageResponse struct {
	Token  TokenReport            `json:"token"`
	Groups map[string]UsageReport `json:"groups"`

	Meta Meta `json:"-"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Stats — the two metered endpoints
// ─────────────────────────────────────────────────────────────────────────────

// Territory is what the API actually resolved, echoed back.
//
// ⚠️ For the geo_id selector Resolution is null, HexCount is 0 and Grain is
// "geo". None of that means "nothing found": the answer came from the stored
// aggregate for the place's own boundary.
type Territory struct {
	Selector   string   `json:"selector"`
	Resolution *int     `json:"resolution"`
	HexCount   int      `json:"hex_count"`
	GeoID      string   `json:"geo_id,omitempty"`
	Grain      string   `json:"grain,omitempty"`
	RadiusKm   *float64 `json:"radius_km,omitempty"`
	Place      *struct {
		Name    string `json:"name"`
		Level   string `json:"level"`
		Country string `json:"country"`
	} `json:"place,omitempty"`
	Center *struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	} `json:"center,omitempty"`
}

// Filters is the filter set AS APPLIED, defaults included — a filter you
// omitted still has a value. ad_sub_type defaults to buy, i.e. sale prices.
type Filters struct {
	AdType      string   `json:"ad_type"`
	AdSubType   string   `json:"ad_sub_type"`
	Rooms       []string `json:"rooms"`
	MinSize     *float64 `json:"min_size"`
	MaxSize     *float64 `json:"max_size"`
	MinPriceUSD *float64 `json:"min_price_usd"`
	MaxPriceUSD *float64 `json:"max_price_usd"`
	Units       struct {
		Size  string `json:"size"`
		Price string `json:"price"`
	} `json:"units"`
	// Note restates that price filtering is USD whatever Currency displays.
	Note string `json:"note"`
}

// Snapshot says WHICH period the figures describe.
type Snapshot struct {
	// AsOf is the period, as its first day. Not today, and never the period
	// in progress.
	AsOf           *string `json:"as_of"`
	Built          string  `json:"built"`
	EarliestPeriod string  `json:"earliest_period"`
}

// Zone labels a cell, truncated to the same cap as ZoneLevel.
type Zone struct {
	Region    *string `json:"region"`
	Province  *string `json:"province"`
	City      *string `json:"city"`
	Macrozone *string `json:"macrozone"`
	Microzone *string `json:"microzone"`
	Country   *string `json:"country"`
}

// Stat is one cell's figures.
//
// Every numeric field is a *float64 rather than a *int64 even where the
// contract says integer: a float64 carries these magnitudes exactly, and a
// server that ever emits 285000.0 would otherwise fail to decode. Nil means
// the aggregate held nothing for that field.
type Stat struct {
	H3 string `json:"h3"`

	// Estimated true means the figures are NOT this cell's own — they are
	// pooled from it and its six neighbours (~1.2 km) by kRing smoothing.
	// Report them as an estimate for the AREA, and never compare a true row
	// against a false one as if they described the same thing.
	Estimated bool `json:"estimated"`

	// LowConfidence is nil when the API did not stamp it, which it only does
	// where a second period read is cheap. ⚠️ ABSENT IS NOT FALSE — test the
	// pointer, never the value.
	LowConfidence *bool `json:"low_confidence"`

	AvgArea    *float64 `json:"avg_area"`
	MedianArea *float64 `json:"median_area"`
	P05Area    *float64 `json:"p05_area"`
	P95Area    *float64 `json:"p95_area"`

	AvgPricePerSqm    *float64 `json:"avg_price_per_sqm"`
	MedianPricePerSqm *float64 `json:"median_price_per_sqm"`
	P05PricePerSqm    *float64 `json:"p05_price_per_sqm"`
	P95PricePerSqm    *float64 `json:"p95_price_per_sqm"`
	MinPricePerSqm    *float64 `json:"min_price_per_sqm"`
	MaxPricePerSqm    *float64 `json:"max_price_per_sqm"`

	AvgPrice    *float64 `json:"avg_price"`
	MedianPrice *float64 `json:"median_price"`
	P05Price    *float64 `json:"p05_price"`
	P95Price    *float64 `json:"p95_price"`
	MinPrice    *float64 `json:"min_price"`
	MaxPrice    *float64 `json:"max_price"`

	// Name is the cell's label, capped by ZoneLevel.
	Name *string `json:"name"`
	// ZoneLevel is the finest level the FIGURES may be attributed to. Do not
	// quote a figure against a level finer than this.
	ZoneLevel *string `json:"zone_level"`
	// Zone is where the cell IS, which is a different question.
	Zone *Zone `json:"zone"`
}

// CurrentStats is /stats/current — METERED, against the current group.
//
// ⚠️ An empty Stats is a 200 and a normal answer: a covered place that holds
// nothing this period. It is not an error and not a zero.
type CurrentStats struct {
	AsOf        *string `json:"as_of"`
	Currency    string  `json:"currency"`
	Resolution  *int    `json:"resolution"`
	Period      *string `json:"period"`
	Granularity string  `json:"granularity"`
	WindowStart *string `json:"window_start"`
	WindowEnd   *string `json:"window_end"`
	FxDate      *string `json:"fx_date"`

	Territory Territory `json:"territory"`
	Filters   Filters   `json:"filters"`
	Stats     []Stat    `json:"stats"`
	Snapshot  Snapshot  `json:"snapshot"`
	// Availability says WHY stats is empty. nil on a server older than
	// 2026-09-20, which did not send it.
	Availability *Availability `json:"availability,omitempty"`

	BinApproximations map[string]json.RawMessage `json:"bin_approximations,omitempty"`

	Meta Meta `json:"-"`
}

// AvailabilityVerdict answers "why is this answer empty". An empty stats array
// is an answer, and four different situations look identical from outside
// until this is read — their remedies are opposites, so an agent that cannot
// tell them apart retries the one that can never succeed.
//
// ⚠️ It describes OUR AGGREGATE, never the market. "No data" means we
// published nothing for this window; it never means nothing was for sale.
type AvailabilityVerdict struct {
	// Status is "ok" when stats has rows, "no_data" when it does not.
	Status string `json:"status"`
	// Reason is null exactly when Status is "ok". The vocabulary is closed:
	// period_not_built, no_data_for_place, below_minimum_sample,
	// no_data_for_selection, not_determined. Treat an unknown value as
	// not_determined, never as an error.
	Reason  *string `json:"reason"`
	Message string  `json:"message"`
}

// Availability is the verdict plus WHEN this territory does hold data — the
// dates turn "empty" into a call that can succeed.
//
// ⚠️ The dates cover the segment (ad_type, ad_sub_type, rooms) and only the
// window SearchedFrom names. A null date means "nothing in the window we
// looked at", never "nothing, ever".
type Availability struct {
	AvailabilityVerdict

	SearchedFrom           *string         `json:"searched_from"`
	EarliestNonemptyPeriod *string         `json:"earliest_nonempty_period"`
	LatestNonemptyPeriod   *string         `json:"latest_nonempty_period"`
	Segment                json.RawMessage `json:"segment,omitempty"`
	Note                   string          `json:"note,omitempty"`
}

// ReasonOrUnknown is the reason, with an absent or null one read as
// not_determined — the API's own instruction for a value it cannot name.
func (v AvailabilityVerdict) ReasonOrUnknown() string {
	if v.Reason == nil || *v.Reason == "" {
		return "not_determined"
	}
	return *v.Reason
}

// HistoryPoint is one period of a series. Each is an independent aggregate
// over its own window.
type HistoryPoint struct {
	Period      string `json:"period"`
	Granularity string `json:"granularity"`
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
	AsOf        string `json:"as_of"`
	FxDate      string `json:"fx_date"`

	BinApproximations map[string]json.RawMessage `json:"bin_approximations,omitempty"`
	Stats             []Stat                     `json:"stats"`
	// Availability is the verdict for this one period. The dates are stated
	// once, on the envelope. nil on a server older than 2026-09-20.
	Availability *AvailabilityVerdict `json:"availability,omitempty"`
}

// HistoryResponse is /stats/history — METERED, against the history group,
// a separate budget from current.
//
// ⚠️ POINTS ARE NOT ADDITIVE. At rolling_3m adjacent windows overlap by two
// months, and a listing live across several periods is a real fact in each.
// Compare points and draw a line; never sum counts across periods.
type HistoryResponse struct {
	Currency   string    `json:"currency"`
	Resolution *int      `json:"resolution"`
	Territory  Territory `json:"territory"`
	Filters    Filters   `json:"filters"`

	Series []HistoryPoint `json:"series"`
	// SeriesNote restates the non-additivity in every response.
	SeriesNote string   `json:"series_note"`
	Snapshot   Snapshot `json:"snapshot"`
	// Availability is the verdict for the series as a whole, plus the dates.
	// nil on a server older than 2026-09-20.
	Availability *Availability `json:"availability,omitempty"`

	Meta Meta `json:"-"`
}
