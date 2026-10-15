// Command investviews is the command-line client for the InvestViews public
// API (https://docs.investviews.ai).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/investviews/investviews-cli/internal/api"
	"github.com/investviews/investviews-cli/internal/commands"
	"github.com/investviews/investviews-cli/internal/config"
	"github.com/investviews/investviews-cli/internal/version"
)

func main() {
	err := newRootCmd().Execute()
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "investviews: "+err.Error())
	// A typed error carries its own recovery action; print it so the next
	// move never has to be guessed.
	if apiErr, ok := api.AsAPIError(err); ok {
		if hint := apiErr.Hint(); hint != "" {
			fmt.Fprintln(os.Stderr, "  "+hint)
		}
	}
	os.Exit(exitCode(err))
}

// exitCode is the process status. 2 is money, 3 is credentials, 4 is a market
// we do not serve, 1 is everything else — a script branches on these, so a new
// error code must not invent a new number.
func exitCode(err error) int {
	if errors.Is(err, errNoToken) {
		return api.ExitAuth
	}
	return commands.ExitCode(err)
}

// errNoToken is returned by "auth status" when nothing is configured, so a
// script can test for it with the exit code.
var errNoToken = errors.New("no token configured")

// newRootCmd builds the command tree. Subcommands that talk to the API (geo,
// stats, usage, coverage) hang off this root and read their token and base URL
// through loadConfig.
func newRootCmd() *cobra.Command {
	var tokenFlag string

	cmd := &cobra.Command{
		Use:   "investviews",
		Short: "Query the InvestViews public API from the shell",
		Long: `investviews queries the InvestViews public API from the shell.

Token, highest precedence first:
  1. the --token flag
  2. the INVESTVIEWS_TOKEN environment variable
  3. ~/.config/investviews/config.toml, written by "investviews auth login"

INVESTVIEWS_API_URL overrides the base URL the CLI talks to. It defaults to
` + config.DefaultAPIURL + ` and accepts any base URL, for example
http://localhost:3000/public/v1 when working against a local server.

Documentation: https://docs.investviews.ai`,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Gives "investviews --version" for free. It prints the same stamp the
		// version subcommand does, so the two can never disagree.
		Version: version.Version,
	}
	cmd.SetVersionTemplate("{{ .Name }} " + version.Version + "\n")

	cmd.PersistentFlags().StringVar(&tokenFlag, "token", "",
		"API token; outranks "+config.EnvToken+" and the config file")
	cmd.AddCommand(newAuthCmd(&tokenFlag), newVersionCmd())
	deps := commands.Deps{NewClient: newClient(&tokenFlag), APIURL: apiURL}
	for _, sub := range commands.All(deps) {
		cmd.AddCommand(sub)
	}

	return cmd
}

// newClient builds the API client one command run uses. The token is resolved
// at run time, not at build time, so --token still wins over the environment
// and the config file.
func newClient(tokenFlag *string) func() (*api.Client, error) {
	return func() (*api.Client, error) {
		cfg, err := loadConfig(*tokenFlag)
		if err != nil {
			return nil, err
		}
		if cfg.Token == "" {
			// Refused before any request: a call with no credential is a
			// 401 that costs a round trip and tells you less than this does.
			return nil, commands.ErrNoToken
		}
		return api.New(api.Options{BaseURL: cfg.APIURL, Token: cfg.Token})
	}
}

// apiURL is the base URL alone, for the hosted MCP mode. It reads the same
// sources as every command (INVESTVIEWS_API_URL, else the default), and ignores
// the token: a hosted server sends each request with that request's token.
func apiURL() (string, error) {
	cfg, err := loadConfig("")
	if err != nil {
		return "", err
	}
	return cfg.APIURL, nil
}

// loadConfig resolves the configuration for one command run.
func loadConfig(tokenFlag string) (*config.Config, error) {
	return config.Load(config.Options{TokenFlag: tokenFlag})
}

// newVersionCmd builds "investviews version": which build this is. It is free,
// offline and needs no token — a support answer must never depend on being able
// to reach the API.
func newVersionCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the build this binary was stamped with (free, offline)",
		Long: `Print the build this binary was stamped with.

A released binary reports its tag, the commit it was built from and the build
time. A binary built from source reports "` + version.Default + `" — that is not
a fault, it means nobody released it.

This command makes no network call and needs no token.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if asJSON {
				body, err := json.MarshalIndent(version.Fields(), "", "  ")
				if err != nil {
					return fmt.Errorf("cannot render the build stamp as JSON: %w", err)
				}
				_, err = fmt.Fprintf(out, "%s\n", body)
				return err
			}
			fmt.Fprintln(out, version.String())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the build stamp as JSON")
	return cmd
}

func newAuthCmd(tokenFlag *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage the stored API token",
	}
	cmd.AddCommand(
		newAuthLoginCmd(tokenFlag),
		newAuthStatusCmd(tokenFlag),
		newAuthLogoutCmd(),
	)
	return cmd
}

func newAuthLoginCmd(tokenFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Store an API token in the config file",
		Long: `Store an API token at ~/.config/investviews/config.toml, mode 0600.

The token is taken from --token, else from ` + config.EnvToken + `, else read
from standard input when it is piped in:

  investviews auth login --token iv_xxx
  echo "$` + config.EnvToken + `" | investviews auth login

Nothing is written if the config file already on disk is readable by the group
or by everyone; fix it with chmod 600 and run the command again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token, source, err := tokenToStore(cmd, *tokenFlag)
			if err != nil {
				return err
			}
			path, err := config.DefaultPath(nil)
			if err != nil {
				return err
			}
			if err := config.SaveToken(path, token); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Stored token %s from %s in %s (mode 0600).\n",
				config.Mask(token), source, path)
			return nil
		},
	}
}

func newAuthStatusCmd(tokenFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether a token is configured and where it came from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*tokenFlag)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			fmt.Fprintf(out, "Config file: %s", cfg.Path)
			if !cfg.FileExists {
				fmt.Fprint(out, " (not present)")
			}
			fmt.Fprintln(out)
			if cfg.InsecureFile() {
				fmt.Fprintf(out, "  warning: mode is %#o and the token is readable beyond you; run: chmod 600 %s\n",
					cfg.FileMode, cfg.Path)
			}
			fmt.Fprintf(out, "Base URL:    %s (%s)\n", cfg.APIURL, cfg.APIURLSource)

			if cfg.Token == "" {
				fmt.Fprintln(out, "Token:       none configured")
				fmt.Fprintf(out, "Run \"investviews auth login --token …\" or set %s.\n", config.EnvToken)
				return errNoToken
			}
			// Masked, always. The full token is never printed.
			fmt.Fprintf(out, "Token:       %s (from %s)\n", config.Mask(cfg.Token), cfg.TokenSource)
			return nil
		},
	}
}

func newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored API token",
		Long: `Remove the token from ~/.config/investviews/config.toml.

A token set through --token or ` + config.EnvToken + ` is not stored on disk, so
this command cannot remove it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.DefaultPath(nil)
			if err != nil {
				return err
			}
			removed, err := config.ClearToken(path)
			if err != nil {
				return err
			}
			if removed {
				fmt.Fprintf(cmd.OutOrStdout(), "Removed the stored token from %s.\n", path)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "No token was stored in %s.\n", path)
			}
			return nil
		},
	}
}

// tokenToStore picks the token "auth login" writes: the flag, else the
// environment, else piped standard input.
func tokenToStore(cmd *cobra.Command, tokenFlag string) (token string, source config.Source, err error) {
	if v := strings.TrimSpace(tokenFlag); v != "" {
		return v, config.SourceFlag, nil
	}
	if v := strings.TrimSpace(os.Getenv(config.EnvToken)); v != "" {
		return v, config.SourceEnv, nil
	}
	if v, ok := tokenFromStdin(cmd.InOrStdin()); ok {
		return v, "standard input", nil
	}
	return "", config.SourceNone, fmt.Errorf(
		"no token given; pass --token, set %s, or pipe the token on standard input", config.EnvToken)
}

// tokenFromStdin reads a piped token. An interactive terminal is left alone so
// the command never appears to hang waiting for input.
func tokenFromStdin(in io.Reader) (string, bool) {
	if f, ok := in.(*os.File); ok {
		info, err := f.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice != 0 {
			return "", false
		}
	}
	b, err := io.ReadAll(in)
	if err != nil {
		return "", false
	}
	token := strings.TrimSpace(string(b))
	return token, token != ""
}
