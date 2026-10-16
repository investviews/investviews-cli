package main

import (
	"testing"

	"github.com/investviews/investviews-cli/internal/config"
)

// The hosted mode runs as a service user that may have no home directory, so
// its API URL must come from the environment alone — never from the config
// file, whose path needs a home.
func TestHostedAPIURLNeedsNoHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	t.Setenv(config.EnvAPIURL, "")
	if got := hostedAPIURL(); got != config.DefaultAPIURL {
		t.Errorf("with no %s: got %q, want %q", config.EnvAPIURL, got, config.DefaultAPIURL)
	}

	t.Setenv(config.EnvAPIURL, "  http://127.0.0.1:3000/public/v1  ")
	if got := hostedAPIURL(); got != "http://127.0.0.1:3000/public/v1" {
		t.Errorf("with %s set: got %q", config.EnvAPIURL, got)
	}
}
