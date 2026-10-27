// Command seed wipes and recreates one installed demo on a Switchboard server
// through the /api/v1 management API. It is the only component that knows a
// demo exists: it writes the demo's MCP token into the server's encrypted secret
// store, creates or reuses the demo's tools, and creates the demo agent with the
// resolved tool pins. Running it twice leaves one live demo.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"govoice/demos"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runCLI(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// runCLI parses the flags and runs one seed. It returns the process exit code:
// 0 success, 1 a failed run, 2 a usage error.
func runCLI(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: seed --server <management API base URL> --demo <name> --mcp <logical>=<base URL> [flags]")
		_, _ = fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	var (
		serverURL string
		keyEnv    string
		demoName  string
		httpBase  string
		wipeOnly  bool
		dryRun    bool
		yes       bool
		jsonOut   bool
		wait      time.Duration
	)
	bases := &mcpFlag{bases: map[string]string{}}
	fs.StringVar(&serverURL, "server", "", "management API base URL of the server (required)")
	fs.StringVar(&keyEnv, "key-env", "GOVOICE_MANAGEMENT_API_KEY", "environment variable holding the management key; its value is never a flag")
	fs.StringVar(&demoName, "demo", "", "demo to seed: dental or atlas (required)")
	fs.Var(bases, "mcp", "base URL of one demo MCP server as <logical>=<base URL>; repeat once per server")
	fs.StringVar(&httpBase, "http-base", "", "base URL for the demo's HTTP tool paths (default: the first --mcp base URL)")
	fs.BoolVar(&wipeOnly, "wipe-only", false, "wipe the demo and stop without creating it")
	fs.BoolVar(&dryRun, "dry-run", false, "list what would happen without writing anything")
	fs.BoolVar(&yes, "yes", false, "confirm wiping an existing demo and overwriting a conflicting HTTP tool definition")
	fs.DurationVar(&wait, "wait", 2*time.Minute, "how long to wait for a pending MCP server deletion")
	fs.BoolVar(&jsonOut, "json", false, "print the report as one JSON document")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "seed: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if serverURL == "" {
		_, _ = fmt.Fprintln(stderr, "seed: --server is required")
		return 2
	}
	if demoName == "" {
		_, _ = fmt.Fprintln(stderr, "seed: --demo is required")
		return 2
	}
	demo, err := demos.Get(demoName)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "seed: %v\n", err)
		return 2
	}
	apiKey := getenv(keyEnv)
	if strings.TrimSpace(apiKey) == "" {
		_, _ = fmt.Fprintf(stderr, "seed: environment variable %s is not set: the management key is read from the environment, never a flag\n", keyEnv)
		return 2
	}
	options := options{
		serverURL: serverURL,
		apiKey:    apiKey,
		keyEnv:    keyEnv,
		demo:      demo,
		mcpBases:  bases.bases,
		httpBase:  httpBase,
		wipeOnly:  wipeOnly,
		dryRun:    dryRun,
		yes:       yes,
		jsonOut:   jsonOut,
		wait:      wait,
		getenv:    getenv,
		log:       stdout,
	}
	report, err := run(ctx, options)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "seed: %v\n", err)
		return 1
	}
	if jsonOut {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			_, _ = fmt.Fprintf(stderr, "seed: encode report: %v\n", err)
			return 1
		}
		return 0
	}
	switch {
	case report.DryRun:
		_, _ = fmt.Fprintf(stdout, "seed: dry run for demo %q: %d action(s), only read-only requests were sent\n", report.Demo, len(report.Actions))
	case report.Created.AgentID == "":
		_, _ = fmt.Fprintf(stdout, "seed: wiped demo %q (%d agent(s) archived, %d MCP server(s) deleted, %d token secret(s) deleted)\n", report.Demo, report.Wiped.AgentsArchived, report.Wiped.MCPServersDeleted, report.Wiped.SecretsDeleted)
	default:
		_, _ = fmt.Fprintf(stdout, "seed: demo %q is installed: agent %s is %s\n", report.Demo, report.Created.AgentID, report.Created.Readiness)
	}
	return 0
}

// mcpFlag collects repeatable --mcp logical=baseURL values.
type mcpFlag struct {
	bases map[string]string
}

func (m *mcpFlag) String() string {
	if m == nil || len(m.bases) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m.bases))
	for logical, base := range m.bases {
		parts = append(parts, logical+"="+base)
	}
	return strings.Join(parts, ", ")
}

func (m *mcpFlag) Set(value string) error {
	logical, base, ok := strings.Cut(value, "=")
	logical, base = strings.TrimSpace(logical), strings.TrimSpace(base)
	if !ok || logical == "" || base == "" {
		return errors.New("value must be <logical>=<base URL>")
	}
	if m.bases == nil {
		m.bases = map[string]string{}
	}
	if _, exists := m.bases[logical]; exists {
		return fmt.Errorf("MCP server %q is named more than once", logical)
	}
	m.bases[logical] = base
	return nil
}
