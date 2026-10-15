package mcpserver

import (
	"context"
	"strings"
	"testing"
)

func TestInitializeReportsNameAndVersion(t *testing.T) {
	session, _ := connectFake(t, nil)

	init := session.InitializeResult()
	if init == nil || init.ServerInfo == nil {
		t.Fatal("initialize returned no server info")
	}
	if got := init.ServerInfo.Name; got != "investviews" {
		t.Errorf("server name = %q, want %q", got, "investviews")
	}
	if got := init.ServerInfo.Version; got != testVersion {
		t.Errorf("server version = %q, want %q", got, testVersion)
	}
	// ⚠️ Without the tools capability a client may never call tools/list.
	if init.Capabilities == nil || init.Capabilities.Tools == nil {
		t.Error("initialize does not advertise the tools capability")
	}
}

// wantTools is the tool set registered so far, in registration order. Each
// task that adds a tool adds its name here.
var wantTools = []string{
	"find_place", "browse_places", "place_at_point",
	"coverage", "filters", "usage",
}

func TestToolsListNamesEveryTool(t *testing.T) {
	session, fake := connectFake(t, nil)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	if len(res.Tools) != len(wantTools) {
		t.Errorf("tools/list returned %d tools, want %d", len(res.Tools), len(wantTools))
	}
	for _, name := range wantTools {
		if !got[name] {
			t.Errorf("tools/list is missing %q", name)
		}
	}
	// Listing tools is protocol-only: it must never reach the API.
	if fake.hits() != 0 {
		t.Errorf("tools/list sent %d API requests, want 0", fake.hits())
	}
}

// TestInitializeCarriesInstructions checks that the agent gets the skill's
// rules with the handshake. An agent connected over MCP may never see
// SKILL.md, so these rules are the only copy it reads: losing one means an
// agent that calls a median "the price" or spends a metered call exploring.
func TestInitializeCarriesInstructions(t *testing.T) {
	session, _ := connectFake(t, nil)

	got := session.InitializeResult().Instructions
	if strings.TrimSpace(got) == "" {
		t.Fatal("initialize returned no instructions")
	}
	for _, rule := range []string{
		"median of asking prices",    // name the statistic
		"median asking price per m²", // always show price per m²
		"Free tools first",           // free before paid
		"find_place",                 // resolve the place with a free tool
		"market_stats",               // the paid call it leads to
		"/reports",                   // never the API's report endpoint
		"Never call it",
		`"~"`, // mark estimates
	} {
		if !strings.Contains(got, rule) {
			t.Errorf("instructions do not contain %q", rule)
		}
	}
}
