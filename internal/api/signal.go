package api

// PlaceSignal is the geo three-state availability signal: prices_available and
// the data-this-period signal read TOGETHER. Separately they are two booleans;
// together they are three answers, plus "not published":
//
//	SignalAsk     — prices + data this period: /stats/current will answer.
//	SignalThin    — prices, nothing THIS period. NOT "no data": /stats/history
//	                may still answer for older windows, and an agent that reads
//	                this as a dead end abandons a live place.
//	SignalDead    — no prices at all. A dead end: do not spend a metered call.
//	SignalUnknown — prices, but this endpoint does not publish the data signal
//	                (a hex page, for one). Says nothing about the place.
//
// ⚠️ THIS IS THE DECISION, NOT THE WORDS. The CLI says "ask stats current"; the
// MCP server says "call market_stats". Both read the same PlaceSignal, so the
// two can word it for their own reader and still never disagree about what a
// place's flags mean. It lives here, not in internal/commands, because
// internal/mcpserver must not import internal/commands (that is a cycle).
type PlaceSignal int

const (
	SignalUnknown PlaceSignal = iota
	SignalAsk
	SignalThin
	SignalDead
)

// String is the machine form, stable enough to put in structured output.
func (s PlaceSignal) String() string {
	switch s {
	case SignalAsk:
		return "ask_current"
	case SignalThin:
		return "try_history"
	case SignalDead:
		return "dead_end"
	default:
		return "unknown"
	}
}

// PlaceAvailability folds a geo row's prices_available, has_data and
// current_period into one PlaceSignal. The data signal is has_data on the
// current API and current_period on one older than 2026-09-20; see
// DataThisPeriod for how the two are read as one.
func PlaceAvailability(pricesAvailable bool, hasData *bool, period NullableDate) PlaceSignal {
	if !pricesAvailable {
		return SignalDead
	}
	switch DataThisPeriod(hasData, period) {
	case DataYes:
		return SignalAsk
	case DataNo:
		return SignalThin
	default:
		return SignalUnknown
	}
}
