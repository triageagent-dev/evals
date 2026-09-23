package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/triageagent-dev/agentevals-go/internal/api"
)

// authCmd dispatches `agentevals auth <subcommand>`. Ported from cli.py's
// `auth` click group - today that's just mint-token, the only subcommand
// under it upstream.
func authCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: agentevals auth mint-token --name <label> [--ttl-days 3650] [--secret ...]")
	}
	switch args[0] {
	case "mint-token":
		return authMintTokenCmd(args[1:])
	default:
		return fmt.Errorf("agentevals auth: unknown subcommand %q (only mint-token exists)", args[0])
	}
}

// authMintTokenCmd ports cli.py's `agentevals auth mint-token`: a signed,
// long-lived bearer token for a non-browser client - the MCP server
// (cmd/agentevals/mcp.go's --session-token), an OTLP exporter, or any
// curl/CI caller - that has no interactive GitHub OAuth login flow of its
// own to mint the agentevals_session cookie the normal way. Accepted
// wherever a session cookie is: internal/api's requireSession Authorization:
// Bearer path, or the cookie itself.
func authMintTokenCmd(args []string) error {
	fs := flag.NewFlagSet("auth mint-token", flag.ExitOnError)
	name := fs.String("name", "", "label for the token (e.g. 'triage-core-otlp' or 'claude-code-mcp'), embedded as the token's subject and logged wherever it authenticates - not a GitHub username, since non-browser clients have no GitHub identity of their own (required)")
	ttlDays := fs.Int("ttl-days", 3650, "token lifetime in days; long-lived by default since machine clients have no login flow to refresh it with - rotate --secret/AGENTEVALS_SESSION_SECRET to invalidate every minted token at once")
	secret := fs.String("secret", os.Getenv("AGENTEVALS_SESSION_SECRET"), "signing secret; falls back to AGENTEVALS_SESSION_SECRET so this can run against the same secret the deployed server uses without pasting it into a flag")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		return fmt.Errorf("--name is required")
	}
	if *secret == "" {
		return fmt.Errorf("no signing secret: pass --secret or set AGENTEVALS_SESSION_SECRET (the same value the running server has)")
	}
	if *ttlDays < 1 {
		return fmt.Errorf("--ttl-days must be at least 1")
	}

	token := api.MintSessionToken(*name, *secret, time.Duration(*ttlDays)*24*time.Hour)
	fmt.Println(token)
	return nil
}
