package commands

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/investviews/investviews-cli/internal/api"
)

// NewUsageCmd builds "investviews usage".
func NewUsageCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "What this token has spent and has left (free)",
		Long: `What this token's account has spent, and what is left, per endpoint group.

This command is FREE and uncounted — checking whether you can still afford a
call never costs one.

⚠️ An uncounted group has NO limit, NO used and NO remaining. They print as an
em dash, never as zero: "0 remaining" on a free group would read as "you are
out", which is the opposite of the truth.

"current" and "history" are separate budgets: spending one does not touch the
other.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
	}
	v := addJSONFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		client, err := deps.client()
		if err != nil {
			return err
		}
		resp, err := client.Usage(cmd.Context())
		if err != nil {
			return err
		}
		return v.emit(cmd, resp, resp.Meta, 1, api.EndpointUsage, func(w io.Writer) {
			renderUsage(w, resp)
		})
	}
	return cmd
}

func renderUsage(w io.Writer, resp *api.UsageResponse) {
	token := resp.Token
	access := "read/write"
	if token.ReadOnly {
		access = "read-only"
	}
	fmt.Fprintf(w, "Token %q (%s) — %s, %s\n", token.Name, api.Dash(token.MaskedToken), api.Dash(token.Status), access)
	if token.LastUsedAt != nil {
		fmt.Fprintf(w, "last used %s\n", *token.LastUsedAt)
	}
	fmt.Fprintln(w)

	names := resp.SortedGroupNames()

	var degraded bool
	table(w, func(tw io.Writer) {
		fmt.Fprintln(tw, "GROUP\tCOST\tUSED\tLIMIT\tREMAINING\tRESETS AT\tRATE/MIN\tSOURCE")
		for _, name := range names {
			g := resp.Groups[name]
			degraded = degraded || g.Degraded
			cost := "free"
			if g.Metered {
				cost = "METERED"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
				name, cost, api.Count(g.Used), api.Count(g.Limit), api.Count(g.Remaining),
				api.DashPtr(g.ResetsAt), g.RatePerMin, api.Dash(g.Source))
		}
	})

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Only stats current (group \"current\") and stats history (group \"history\") spend")
	fmt.Fprintln(w, "quota. Everything under geo, plus coverage and this command, is uncounted.")
	if degraded {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "⚠️ degraded: the metering store was unreachable, so these figures are the")
		fmt.Fprintln(w, "committed balance alone and may lag the truth. Requests are still being served.")
	}
}
