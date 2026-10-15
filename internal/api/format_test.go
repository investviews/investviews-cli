package api

import (
	"reflect"
	"testing"
)

// ⚠️ A missing figure is an em dash, never a zero: the aggregate held nothing
// for that field, which is not the number 0.
func TestNumNeverTurnsAMissingFigureIntoZero(t *testing.T) {
	if got := Num(nil); got != "—" {
		t.Errorf("Num(nil) = %q", got)
	}
	zero := 0.0
	if got := Num(&zero); got != "0" {
		t.Errorf("a real zero must still print: %q", got)
	}
	value := 3120.456
	if got := Num(&value); got != "3120.46" {
		t.Errorf("Num = %q", got)
	}
}

func TestCountNeverTurnsAnUncountedGroupIntoZero(t *testing.T) {
	if got := Count(nil); got != "—" {
		t.Errorf("Count(nil) = %q", got)
	}
	var zero int64
	if got := Count(&zero); got != "0" {
		t.Errorf("a real zero must still print: %q", got)
	}
}

func TestDashForms(t *testing.T) {
	empty, set := "", "x"
	cases := []struct{ got, want string }{
		{Dash("  "), "—"}, {Dash("a"), "a"},
		{DashPtr(nil), "—"}, {DashPtr(&empty), "—"}, {DashPtr(&set), "x"},
		{YesNo(true), "yes"}, {YesNo(false), "no"},
		{Resolutions(nil), "—"}, {Resolutions([]int{4, 6}), "4,6"},
	}
	for _, c := range cases {
		if got, want := c.got, c.want; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestSortedGroupNamesPutsMeteredGroupsFirst(t *testing.T) {
	r := &UsageResponse{Groups: map[string]UsageReport{
		"zeta": {}, "metadata": {}, "alpha": {}, "history": {}, "reports": {}, "current": {},
	}}
	want := []string{"current", "history", "reports", "metadata", "alpha", "zeta"}
	if got := r.SortedGroupNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("SortedGroupNames = %v, want %v", got, want)
	}
}

func TestQuotaDetailIsEmptyUnlessMetered(t *testing.T) {
	used := int64(12)
	cases := []struct {
		meta Meta
		want string
	}{
		{Meta{}, ""},
		{Meta{QuotaGroup: "metadata", QuotaLimit: Uncounted}, ""},
		{Meta{QuotaGroup: "current", QuotaLimit: "100", QuotaUsed: &used}, "(group current, 12 of 100 used this cycle)"},
		{Meta{QuotaGroup: "current", QuotaLimit: "100"}, "(group current, limit 100)"},
	}
	for _, c := range cases {
		if got := c.meta.QuotaDetail(); got != c.want {
			t.Errorf("QuotaDetail(%+v) = %q, want %q", c.meta, got, c.want)
		}
	}
}
