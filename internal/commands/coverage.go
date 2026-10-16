package commands

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/investviews/investviews-cli/internal/api"
)

// NewCoverageCmd builds "investviews coverage".
func NewCoverageCmd(deps Deps) *cobra.Command {
	var country string

	cmd := &cobra.Command{
		Use:   "coverage",
		Short: "Which markets this API serves (free)",
		Long: `Which markets this API serves, and how fresh each one is.

  investviews coverage
  investviews coverage --country es

FREE and uncounted, and computed live per request.

⚠️ "prices" and "searchable" answer DIFFERENT questions, and the gap between
them is the useful part: a market can hold listings and still name nothing, so
a place there is reachable by cell or by coordinate but not by name.

⚠️ A blank "last ad" means NO SOURCE TARGETS THAT MARKET DIRECTLY. It does not
mean the market is empty.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
	}
	v := addJSONFlag(cmd)
	cmd.Flags().StringVar(&country, "country", "", "restrict to one ISO 3166-1 alpha-2 code (es)")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		client, err := deps.client()
		if err != nil {
			return err
		}
		resp, err := client.Coverage(cmd.Context(), api.CoverageParams{Country: country})
		if err != nil {
			return err
		}
		return v.emit(cmd, resp, resp.Meta, 1, api.EndpointCoverage, func(w io.Writer) {
			renderCoverage(w, country, resp)
		})
	}
	return cmd
}

func renderCoverage(w io.Writer, country string, resp *api.CoverageResponse) {
	fmt.Fprintf(w, "%d market(s), measured over a %d-day window.\n", len(resp.Countries), resp.WindowDays)
	if note := strings.TrimSpace(resp.Freshness); note != "" {
		fmt.Fprintf(w, "freshness: %s\n", note)
	}
	if note := strings.TrimSpace(resp.Coverage); note != "" {
		fmt.Fprintf(w, "coverage:  %s\n", note)
	}
	fmt.Fprintln(w)

	if len(resp.Countries) == 0 {
		if strings.TrimSpace(country) != "" {
			fmt.Fprintf(w, "No row for %q. That is the honest answer to a well-formed question: the\n", country)
			fmt.Fprintln(w, "code is valid and this API holds no market for it. Run without --country to")
			fmt.Fprintln(w, "see the markets that are served.")
			return
		}
		fmt.Fprintln(w, "No markets returned.")
		return
	}

	table(w, func(tw io.Writer) {
		fmt.Fprintln(tw, "COUNTRY\tPRICES\tSEARCHABLE\tREPORTS\tRESOLUTIONS\tLAST AD PARSED")
		for _, row := range resp.Countries {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				row.Country, api.YesNo(row.PricesAvailable), api.YesNo(row.Searchable), api.YesNo(row.ReportsAvailable),
				api.Resolutions(row.H3Resolutions), api.DashPtr(row.LastAdParsedAt))
		}
	})

	fmt.Fprintln(w)
	fmt.Fprintln(w, "A code in the COUNTRY column is a valid --geo-id for stats: `investviews stats")
	fmt.Fprintln(w, "current --geo-id es` answers for the whole market in one metered call.")
	fmt.Fprintln(w, "A blank LAST AD PARSED means no source targets that market directly — not empty.")
}
