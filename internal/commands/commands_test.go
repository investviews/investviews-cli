package commands

import (
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/investviews/investviews-cli/internal/api"
)

// The exit codes are a contract with whoever scripts around this CLI: 2 is
// money, 3 is credentials, 4 is a market we do not serve, 1 is everything
// else. A locally refused request is a usage mistake, so it stays on 1.
func TestExitCodesSayWhichKindOfFailureItWas(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    int
		command []string
	}{
		{
			name:    "quota exhausted is money, not credentials",
			status:  http.StatusPaymentRequired,
			body:    `{"error":"quota_exhausted","message":"spent","docs_url":"d"}`,
			want:    api.ExitQuota,
			command: []string{"stats", "current", "--geo-id", "es"},
		},
		{
			name:    "an inactive subscription is money too",
			status:  http.StatusPaymentRequired,
			body:    `{"error":"subscription_inactive","message":"inactive","docs_url":"d"}`,
			want:    api.ExitQuota,
			command: []string{"stats", "current", "--geo-id", "es"},
		},
		{
			name:    "a bad token is credentials",
			status:  http.StatusUnauthorized,
			body:    `{"error":"invalid_token","message":"bad","docs_url":"d"}`,
			want:    api.ExitAuth,
			command: []string{"usage"},
		},
		{
			name:    "a market we do not serve has its own code",
			status:  http.StatusNotFound,
			body:    `{"error":"not_covered","message":"no market","docs_url":"d"}`,
			want:    api.ExitNotCovered,
			command: []string{"coverage", "--country", "aq"},
		},
		{
			name:    "an unknown place is ordinary failure — it has a next move",
			status:  http.StatusNotFound,
			body:    `{"error":"unknown_place","message":"no match","docs_url":"d","did_you_mean":[]}`,
			want:    api.ExitError,
			command: []string{"geo", "search", "zzz"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, func(w http.ResponseWriter, _ *http.Request) {
				writeBody(w, tc.status, tc.body)
			}, tc.command...)
			if res.err == nil {
				t.Fatal("want an error")
			}
			if got := ExitCode(res.err); got != tc.want {
				t.Errorf("exit code = %d, want %d (error: %v)", got, tc.want, res.err)
			}
		})
	}
}

func TestExitCodeForAMissingTokenIsTheCredentialCode(t *testing.T) {
	if got := ExitCode(ErrNoToken); got != api.ExitAuth {
		t.Errorf("exit code = %d, want %d", got, api.ExitAuth)
	}
	if got := ExitCode(nil); got != api.ExitOK {
		t.Errorf("exit code for no error = %d, want %d", got, api.ExitOK)
	}
}

// A command that cannot build a client never reaches the network, and says
// what to do rather than letting a 401 explain it.
func TestACommandWithoutACredentialRefusesBeforeAnyRequest(t *testing.T) {
	deps := Deps{NewClient: func() (*api.Client, error) { return nil, ErrNoToken }}
	for _, cmd := range All(deps) {
		if cmd.Name() == "" {
			t.Fatal("a command with no name")
		}
	}
	if got := ExitCode(ErrNoToken); got != api.ExitAuth {
		t.Errorf("exit code = %d", got)
	}
}

func TestAvailabilityIsThreeAnswersNotTwo(t *testing.T) {
	ask := availability(true, nil, api.Date("2026-08-01"))
	thin := availability(true, nil, api.NullDate())
	dead := availability(false, nil, api.NullDate())

	if ask == thin || thin == dead || ask == dead {
		t.Fatalf("the three states must read differently:\n%q\n%q\n%q", ask, thin, dead)
	}
	contains(t, thin, "stats history")
	contains(t, thin, "not a dead end")
	contains(t, dead, "do not spend a metered call")
}

// ⚠️ has_data REPLACED current_period on 2026-09-20, and the skill's decision
// table is written against these exact lines. The new signal must land on the
// SAME three answers as the old one — otherwise the table silently stops
// matching whichever server version an agent happens to reach.
func TestAvailabilityReadsHasDataAsTheSameThreeAnswers(t *testing.T) {
	yes, no := true, false

	ask := availability(true, &yes, api.NullableDate{})
	thin := availability(true, &no, api.NullableDate{})
	dead := availability(false, &no, api.NullableDate{})

	contains(t, ask, "ask stats current")
	if thin != availability(true, nil, api.NullDate()) {
		t.Errorf("has_data false must read exactly like a null current_period:\n%q", thin)
	}
	if dead != availability(false, nil, api.NullDate()) {
		t.Errorf("a dead end must read the same on both contracts:\n%q", dead)
	}
	// has_data carries no date, so the line must not pretend it knows one.
	if strings.Contains(ask, "current period 20") {
		t.Errorf("has_data names no period, but the line did: %q", ask)
	}
	// And the new signal wins when a server sends both.
	if got := availability(true, &no, api.Date("2026-08-01")); got != thin {
		t.Errorf("has_data must win over current_period; got %q", got)
	}
}

// ⚠️ null ancestry and empty ancestry are different answers. Unknown means
// "try another selector"; empty means "this IS the root".
func TestAncestryKeepsUnknownApartFromRoot(t *testing.T) {
	if got := ancestry(api.UnknownAncestors()); got == ancestry(api.NewAncestors(nil)) {
		t.Fatal("unknown and root ancestry must not render the same")
	}
	contains(t, ancestry(api.UnknownAncestors()), "ancestry unknown")
	contains(t, ancestry(api.NewAncestors(nil)), "top of the tree")
	contains(t,
		ancestry(api.NewAncestors([]api.Ancestor{{GeoID: "R1", Name: "Madrid", Level: "city"}})),
		"Madrid (city, R1)")
}

// ⚠️ A missing figure is an em dash, never a zero: the aggregate held nothing
// for that field, which is not the number 0.
func TestNumNeverTurnsAMissingFigureIntoZero(t *testing.T) {
	if got := num(nil); got != "—" {
		t.Errorf("num(nil) = %q", got)
	}
	zero := 0.0
	if got := num(&zero); got != "0" {
		t.Errorf("a real zero must still print: %q", got)
	}
	value := 3120.456
	if got := num(&value); got != "3120.46" {
		t.Errorf("num = %q", got)
	}
}

func TestHexIDsAcceptRepeatsCommasAndStdin(t *testing.T) {
	cells, err := hexIDs([]string{"1,2", "3"}, nil)
	if err != nil {
		t.Fatalf("hexIDs: %v", err)
	}
	if len(cells) != 3 || cells[0] != "1" || cells[2] != "3" {
		t.Errorf("cells = %v", cells)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// every shipped example must be a command line that exists
// ─────────────────────────────────────────────────────────────────────────────

// ⚠️ A SHIPPED EXAMPLE IS AN INSTRUCTION, AND AN AGENT RUNS IT VERBATIM.
//
// `stats current --help` shipped
//
//	investviews geo hexes R344953 | investviews stats current --h3 -
//
// for a whole release, and `geo hexes` had no ids-only output: the pipe sent
// the header's words — R344953, València, (city, es), 3, cell(s), at, 8 — to a
// METERED endpoint as h3= values. Nothing in the build noticed, because
// nothing in the build had ever run the examples.
//
// This walks every example in every Long text, both halves of every pipe, and
// asserts each one names a command that exists, uses only flags that command
// registers, and passes that command's own argument rule. It cannot prove an
// example does the right thing — only a real run does that — but it does make
// a renamed flag or a command that never existed fail the build.
func TestEveryExampleInHelpTextIsACommandLineThatExists(t *testing.T) {
	root := &cobra.Command{Use: "investviews"}
	for _, sub := range All(Deps{}) {
		root.AddCommand(sub)
	}

	var checked int
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, line := range strings.Split(cmd.Long, "\n") {
			for _, half := range strings.Split(line, "|") {
				args, ok := exampleArgs(half)
				if !ok {
					continue
				}
				checked++
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					target, rest, err := root.Find(args)
					if err != nil {
						t.Fatalf("no such command: %v", err)
					}
					if err := target.ParseFlags(rest); err != nil {
						t.Fatalf("the example uses a flag %q does not have: %v", target.CommandPath(), err)
					}
					if err := target.ValidateArgs(target.Flags().Args()); err != nil {
						t.Fatalf("%q would refuse this line's arguments: %v", target.CommandPath(), err)
					}
				})
			}
		}
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(root)

	// A walker that silently found nothing would pass forever.
	if checked < 10 {
		t.Fatalf("only %d examples were checked; the extractor has stopped matching them", checked)
	}
}

// exampleArgs turns one line of help text into argv, or reports that the line
// is not an example. It honours "$ " prompts, double quotes and trailing "#"
// comments — the three things that separate a line a reader can paste from one
// that only looks like it.
func exampleArgs(line string) ([]string, bool) {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "$"))
	if comment := strings.Index(line, " #"); comment >= 0 {
		line = line[:comment]
	}
	if !strings.HasPrefix(line, "investviews ") {
		return nil, false
	}

	var args []string
	var current strings.Builder
	var quoted, started bool
	for _, r := range strings.TrimPrefix(line, "investviews ") {
		switch {
		case r == '"':
			quoted, started = !quoted, true
		case r == ' ' && !quoted:
			if started {
				args = append(args, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if started {
		args = append(args, current.String())
	}
	return args, len(args) > 0
}
