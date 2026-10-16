package api

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// The pure value formatters the CLI's tables and the MCP tools' text both use.
// They live here, not in internal/commands, because internal/mcpserver must not
// import internal/commands (internal/commands/mcp.go imports it).
//
// Only the VALUE rendering is shared. The sentences around the values differ
// on purpose between the CLI (a person reading a terminal) and the MCP tools
// (an agent choosing its next call), so the line renderers stay in each
// package.
//
// ⚠️ A MISSING VALUE IS AN EM DASH, NEVER A ZERO. A nil figure means the
// aggregate held nothing; a nil counter means the group is uncounted. "0"
// would read as a real price, or as "you are out".

// Dash renders an empty or blank string as an em dash, so a blank field is
// visibly blank rather than looking like a layout bug.
func Dash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "—"
	}
	return v
}

// DashPtr renders an optional string: nil or "" is an em dash.
func DashPtr(v *string) string {
	if v == nil || *v == "" {
		return "—"
	}
	return *v
}

// Num renders an optional figure rounded to cents, with no trailing zeros.
func Num(v *float64) string {
	if v == nil {
		return "—"
	}
	rounded := math.Round(*v*100) / 100
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}

// Count renders an optional counter. nil is unknown or uncounted, never 0.
func Count(v *int64) string {
	if v == nil {
		return "—"
	}
	return strconv.FormatInt(*v, 10)
}

// YesNo renders a flag as yes or no.
func YesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// Resolutions renders a list of H3 resolutions as "4,5,6", or an em dash.
func Resolutions(values []int) string {
	if len(values) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ",")
}

// usageGroupOrder is the order usage groups are listed in: the two that cost
// money first, then the two that do not.
var usageGroupOrder = map[string]int{"current": 0, "history": 1, "reports": 2, "metadata": 3}

// SortedGroupNames is the group names in display order: current, history,
// reports, metadata, then any group this build does not know, by name.
//
// Reading a map in Go is unordered, and a list whose rows moved between calls
// would be unreadable to a person and unquotable by an agent.
func (r *UsageResponse) SortedGroupNames() []string {
	names := make([]string, 0, len(r.Groups))
	for name := range r.Groups {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		oi, iKnown := usageGroupOrder[names[i]]
		oj, jKnown := usageGroupOrder[names[j]]
		switch {
		case iKnown && jKnown:
			return oi < oj
		case iKnown != jKnown:
			return iKnown
		default:
			return names[i] < names[j]
		}
	})
	return names
}
