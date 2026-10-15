package mcpserver

import (
	"context"
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

func TestToolsListIsEmpty(t *testing.T) {
	session, fake := connectFake(t, nil)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(res.Tools) != 0 {
		t.Errorf("tools/list returned %d tools, want 0", len(res.Tools))
	}
	// Listing tools is protocol-only: it must never reach the API.
	if fake.hits() != 0 {
		t.Errorf("tools/list sent %d API requests, want 0", fake.hits())
	}
}
