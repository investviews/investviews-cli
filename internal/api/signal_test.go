package api

import "testing"

// ⚠️ The three states must stay three, on both contracts. has_data replaced
// current_period on 2026-09-20; a place must land on the same answer whichever
// field the server sent, or the CLI's lines and the MCP server's structured
// output drift apart depending on which API version an agent reaches.
func TestPlaceAvailabilityIsThreeAnswersOnBothContracts(t *testing.T) {
	yes, no := true, false

	cases := []struct {
		name    string
		prices  bool
		hasData *bool
		period  NullableDate
		want    PlaceSignal
	}{
		{"has_data true", true, &yes, NullableDate{}, SignalAsk},
		{"current_period set", true, nil, Date("2026-08-01"), SignalAsk},
		{"has_data false", true, &no, NullableDate{}, SignalThin},
		{"current_period null", true, nil, NullDate(), SignalThin},
		{"no prices, has_data false", false, &no, NullableDate{}, SignalDead},
		{"no prices, null period", false, nil, NullDate(), SignalDead},
		// prices_available false is a dead end even if a data flag says yes.
		{"no prices wins over has_data", false, &yes, NullableDate{}, SignalDead},
		// has_data is the newer contract and wins when both are sent.
		{"has_data wins over a period", true, &no, Date("2026-08-01"), SignalThin},
		{"neither field sent", true, nil, NullableDate{}, SignalUnknown},
	}
	for _, c := range cases {
		if got := PlaceAvailability(c.prices, c.hasData, c.period); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPlaceSignalStringsAreDistinct(t *testing.T) {
	seen := map[string]PlaceSignal{}
	for _, s := range []PlaceSignal{SignalUnknown, SignalAsk, SignalThin, SignalDead} {
		name := s.String()
		if prev, dup := seen[name]; dup {
			t.Fatalf("%v and %v both render as %q", prev, s, name)
		}
		seen[name] = s
	}
}
