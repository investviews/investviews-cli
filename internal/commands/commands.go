// Package commands is the investviews command tree: the cobra commands that
// turn flags into internal/api calls, and API answers into text an agent can
// read, act on and cite.
//
// Four things in here exist because getting them wrong produces a CLI that
// looks right and misleads:
//
//  1. --level MEANS TWO DIFFERENT THINGS, and which one depends on --parent.
//     Under a country it names a whole level of that country; under a place it
//     filters that place's direct children. Both meanings are in the flag's
//     help text AND stated again in the header line of every result, so the
//     output says which move actually happened.
//
//  2. AN EMPTY RESULT IS USUALLY THE CORRECT ANSWER. Levels are skipped, not
//     shifted, so a city's microzones legitimately do not exist. Every empty
//     listing prints as a normal answer with a next move, and exits 0.
//
//  3. FREE AND METERED CALLS MUST NOT LOOK ALIKE. Only stats current|history
//     spend quota. The server reports it per response in X-Quota-Group and
//     X-Quota-Limit, so the cost line is built from what the server said, not
//     from a list in this package; the endpoint's documented group is only the
//     fallback for a response that carried no quota headers at all.
//
//  4. THE THREE-STATE AVAILABILITY SIGNAL is not a boolean. prices_available
//     with a current_period is "ask"; prices_available with a null period is
//     "too thin THIS period, try history"; no prices_available is "stop".
package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/investviews/investviews-cli/internal/api"
)

// Deps is what the root command hands every data command: one way to build an
// API client for one run. Tests replace it with a client pointed at an
// httptest server, which is why no command in here builds its own.
type Deps struct {
	NewClient func() (*api.Client, error)
}

func (d Deps) client() (*api.Client, error) {
	if d.NewClient == nil {
		return nil, errors.New("internal: no API client factory was configured")
	}
	return d.NewClient()
}

// ErrNoToken is returned before any request when no credential is configured.
// It never reaches the network, so it costs nothing and says so.
var ErrNoToken = errors.New(
	"no API token configured; run \"investviews auth login --token …\" or set INVESTVIEWS_TOKEN")

// ExitCode is the process status for any error a command returns. It defers to
// the API package for everything the server said, and adds the one condition
// this package raises on its own.
func ExitCode(err error) int {
	if errors.Is(err, ErrNoToken) {
		return api.ExitAuth
	}
	return api.ExitCode(err)
}

// All builds every data command. The root command adds them next to auth.
func All(deps Deps) []*cobra.Command {
	return []*cobra.Command{
		NewGeoCmd(deps),
		NewStatsCmd(deps),
		NewUsageCmd(deps),
		NewCoverageCmd(deps),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// output
// ─────────────────────────────────────────────────────────────────────────────

// view is the one output decision every data command shares: the shape.
type view struct {
	asJSON bool

	// bare marks a TEXT shape written for a pipe rather than for a reader:
	// nothing on stdout but the values themselves. It moves the cost line
	// to stderr for exactly the reason --json does — a stream another
	// command parses must not have prose mixed into it. `geo hexes
	// --ids-only` is the only user of it, and it exists because the
	// documented pipe into `stats current --h3 -` was being fed the header,
	// the availability sentence and the cost line as if they were cell ids.
	bare bool
}

// addJSONFlag registers --json and returns the view the command reads at run
// time.
func addJSONFlag(cmd *cobra.Command) *view {
	v := &view{}
	cmd.Flags().BoolVar(&v.asJSON, "json", false,
		"print the response as JSON instead of text; the cost line then goes to stderr, so stdout stays pipeable")
	return v
}

// emit writes the answer in the requested shape and, always, the cost line.
//
// ⚠️ The cost line is not optional and not a footnote: a free call and a
// metered one must never look alike in the output. In --json mode it goes to
// STDERR so that stdout is nothing but the payload.
func (v view) emit(cmd *cobra.Command, payload any, meta api.Meta, requests int, endpoint api.Endpoint, text func(io.Writer)) error {
	cost := costLine(meta, requests, endpoint)
	if v.asJSON {
		if err := writeJSON(cmd.OutOrStdout(), payload); err != nil {
			return err
		}
		fmt.Fprintln(cmd.ErrOrStderr(), cost)
		return nil
	}
	out := cmd.OutOrStdout()
	text(out)
	if v.bare {
		// The cost line still goes out — a free call and a metered one must
		// never look alike, whatever shape the payload is in.
		fmt.Fprintln(cmd.ErrOrStderr(), cost)
		return nil
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, cost)
	return nil
}

func writeJSON(w io.Writer, payload any) error {
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot render the response as JSON: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", body)
	return err
}

// costLine says what this run spent, in the words the SERVER used.
//
// ⚠️ The quota headers are the authoritative answer to "did this call spend
// quota" — an endpoint's documented group is only what we expected. They are
// absent in exactly two situations: nginx's edge answers (Rails never ran) and
// a paged walk that ended before any page came back. Then, and only then, this
// falls back to the contract's group and labels the answer "expected".
func costLine(meta api.Meta, requests int, endpoint api.Endpoint) string {
	if requests < 1 {
		requests = 1
	}
	if !meta.HasQuotaHeaders() {
		kind := "FREE"
		if endpoint.Metered() {
			kind = "METERED"
		}
		return fmt.Sprintf("cost: %s (expected — group %s; this response carried no quota headers) · %s",
			kind, endpoint.Group(), plural(requests))
	}
	if !meta.Metered() {
		return fmt.Sprintf("cost: FREE — group %s, uncounted · %s", meta.QuotaGroup, plural(requests))
	}
	if meta.QuotaUsed != nil {
		return fmt.Sprintf("cost: METERED — group %s · %s · %d of %s used this cycle",
			meta.QuotaGroup, plural(requests), *meta.QuotaUsed, meta.QuotaLimit)
	}
	return fmt.Sprintf("cost: METERED — group %s · %s · limit %s",
		meta.QuotaGroup, plural(requests), meta.QuotaLimit)
}

func plural(n int) string {
	if n == 1 {
		return "1 request"
	}
	return fmt.Sprintf("%d requests", n)
}

// table writes an aligned block. Column separators are tabs, so a caller
// writes "a\tb\tc\n" and the widths are worked out here.
func table(w io.Writer, write func(io.Writer)) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	write(tw)
	_ = tw.Flush()
}

// ─────────────────────────────────────────────────────────────────────────────
// the three-state availability signal
// ─────────────────────────────────────────────────────────────────────────────

// availability renders prices_available and the data signal TOGETHER, because
// separately they are two booleans and together they are three answers:
//
//	true  + data   — /stats/current will answer this place.
//	true  + none   — too thin THIS period. NOT "no data": /stats/history may
//	                 still answer for older windows, and an agent that reads
//	                 this as a dead end abandons a live place.
//	false + none   — a dead end. Do not spend a metered call.
//
// The data signal is has_data on the current API and current_period on one
// older than 2026-09-20 (see api.DataThisPeriod). The words are the same for
// both, so the skill's table does not depend on which server answered — only
// the old API names the period, and then the line names it too.
func availability(pricesAvailable bool, hasData *bool, period api.NullableDate) string {
	if !pricesAvailable {
		return "no prices — dead end, do not spend a metered call"
	}
	switch api.DataThisPeriod(hasData, period) {
	case api.DataYes:
		if hasData == nil {
			return "prices, current period " + period.Value() + " — ask stats current"
		}
		return "prices, data in the current period — ask stats current"
	case api.DataNo:
		return "prices, but nothing in the current period — try stats history, not a dead end"
	default:
		// Neither key was sent. This endpoint does not publish the signal,
		// which says nothing about the place either way.
		return "prices — this endpoint does not publish a current period"
	}
}

// dataNow is the one-word column form of the same signal, for tables where a
// sentence per row would not fit.
func dataNow(hasData *bool, period api.NullableDate) string {
	switch api.DataThisPeriod(hasData, period) {
	case api.DataYes:
		if hasData == nil {
			return period.Value()
		}
		return "yes"
	case api.DataNo:
		return "no — try history"
	default:
		return "—"
	}
}

// ancestry renders the chain fine to coarse, WITH the ids.
//
// ⚠️ Names repeat: 12,233 of 89,965 microzone names spanned two or more parent
// cities when that was last measured, in 2026-08 against h3_zone_lookup and
// before KAN-200 (it has not been re-measured since). So two hits can be
// identical on name, level and country, and this chain — with an id at every
// step — is the only thing that tells them apart or lets an agent act on the
// difference.
//
// ⚠️ null ancestry is not an empty one. Unknown means "we hold no hierarchy
// row for this zone, try another selector"; empty means "this IS the root".
func ancestry(a api.Ancestors) string {
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

// ─────────────────────────────────────────────────────────────────────────────
// values
// ─────────────────────────────────────────────────────────────────────────────

// num renders an optional figure. ⚠️ A missing figure is an em dash, never a
// zero: the aggregate held nothing for that field, which is not the number 0.
func num(v *float64) string {
	if v == nil {
		return "—"
	}
	rounded := math.Round(*v*100) / 100
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}

func str(v *string) string {
	if v == nil || *v == "" {
		return "—"
	}
	return *v
}

// dash renders an empty string as an em dash, so a blank column is visibly
// blank rather than looking like a layout bug.
func dash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "—"
	}
	return v
}

// splitList expands a repeatable flag that also accepts comma-joined values,
// so --h3 a --h3 b,c and --h3 a,b,c are the same request.
func splitList(values []string) []string {
	var out []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// readList reads whitespace- or comma-separated values from r, for the "-"
// form that pipes a cell list in.
func readList(r io.Reader) ([]string, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("cannot read the list from standard input: %w", err)
	}
	fields := strings.FieldsFunc(string(body), func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out, nil
}

// looksLikeCountryCode reports the two-letter form. geo_id addresses both a
// country (es) and a place (R344953) through one parameter, and the two
// behave differently under --level, so the shape is worth naming.
func looksLikeCountryCode(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 2 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}
