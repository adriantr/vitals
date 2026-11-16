// Command vitals prints a compact health snapshot of a Google Cloud estate for agents and people:
// open and flapping alerts, SLO burn, new errors, Pub/Sub backlog, deploy failures and recent deploys.
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
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/adriantr/vitals/internal/component"
	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/drift"
	"github.com/adriantr/vitals/internal/explain"
	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
	"github.com/adriantr/vitals/internal/snapshot"
	"github.com/adriantr/vitals/internal/source"
	"github.com/adriantr/vitals/internal/topology"
)

const usage = `usage:
  vitals snapshot [--since 6h] [--at TIME] [--slo] [--env NAME] [--json] [--config FILE]
  vitals component <service> [--since 6h] [--at TIME] [--env NAME] [--json] [--config FILE]
  vitals check [--env NAME] [--config FILE]
  vitals suggest [--env NAME] [--config FILE]
  vitals map [--json] [--config FILE]
  vitals explain <ref> [--since 24h] [--at TIME] [--env NAME] [--json] [--config FILE]
  vitals changes [--since 24h] [--at TIME] [--env NAME] [--json] [--config FILE]
  vitals version

  snapshot   every system and service at a glance: status, release, traffic, signals, findings, changes
  component  one service: declaration, release, signals, dependencies, metrics and findings
  check      compares the config with what is deployed and alerting; exits 1 on drift (for CI)
  suggest    prints YAML stubs for undeclared services, unclaimed alert policies and missing log metrics
  map        the static system map read from the repository (needs topology:; no cloud reads)

  <ref> is a finding's ref: policy:<display name>, slo:<service>/<slo>, error:<group id>,
  pubsub:<subscription>, workflow:<file> or signal:<owner>/<signal> [k=v ...].
  --since takes 30m, 6h, 2d or any Go duration, up to 7d. --at (RFC 3339) ends the window
  in the past instead of now.
  --config defaults to $VITALS_CONFIG, then ./vitals.yaml.
exit: 0 every source answered (check: no drift), 1 a source was BLOCKED, FAILED or SKIPPED (check:
drift found), 2 bad arguments or config`

// version is set at release time with -ldflags "-X main.version=..."; go install builds fall back
// to the module version.
var version = ""

func versionString() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

const maxWindow = 7 * 24 * time.Hour

var errUsage = errors.New("usage")

type options struct {
	command string
	ref     string
	env     string
	since   time.Duration
	at      time.Time
	json    bool
	slo     bool
	config  string
}

func parse(args []string) (options, error) {
	if len(args) == 0 {
		return options{}, fmt.Errorf("%w: missing command", errUsage)
	}
	opts := options{command: args[0]}
	defaultSince := "24h"
	switch opts.command {
	case "snapshot", "component":
		defaultSince = "6h"
	case "explain", "changes", "map", "check", "suggest":
	case "version", "--version":
		return opts, nil
	case "-h", "--help", "help":
		return options{}, errUsage
	default:
		return options{}, fmt.Errorf("%w: unknown command %q", errUsage, opts.command)
	}
	fs := flag.NewFlagSet(opts.command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	since := fs.String("since", defaultSince, "")
	at := fs.String("at", "", "")
	fs.StringVar(&opts.env, "env", "", "")
	fs.BoolVar(&opts.json, "json", false, "")
	if opts.command == "snapshot" {
		fs.BoolVar(&opts.slo, "slo", false, "")
	}
	fs.StringVar(&opts.config, "config", "", "")
	// Flags may sit on either side of the explain or component argument.
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			return options{}, fmt.Errorf("%w: %w", errUsage, err)
		}
		if fs.NArg() == 0 {
			break
		}
		if opts.ref != "" || (opts.command != "explain" && opts.command != "component") {
			return options{}, fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
		}
		opts.ref, rest = fs.Arg(0), fs.Args()[1:]
	}
	if opts.command == "explain" && opts.ref == "" {
		return options{}, fmt.Errorf("%w: explain needs a <ref> from a snapshot", errUsage)
	}
	if opts.command == "component" && opts.ref == "" {
		return options{}, fmt.Errorf("%w: component needs a service name", errUsage)
	}
	opts.at = time.Now().UTC()
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil || t.After(opts.at) {
			return options{}, fmt.Errorf("%w: --at must be an RFC 3339 time in the past, got %q", errUsage, *at)
		}
		opts.at = t.UTC()
	}
	d, err := parseSince(*since)
	if err != nil || d < time.Minute || d > maxWindow {
		return options{}, fmt.Errorf("%w: --since must be a duration between 1m and 7d, got %q", errUsage, *since)
	}
	opts.since = d
	if opts.config == "" {
		opts.config = os.Getenv("VITALS_CONFIG")
	}
	if opts.config == "" {
		opts.config = "vitals.yaml"
	}
	return opts, nil
}

// parseSince accepts Go durations plus a whole-day form such as 2d, which time.ParseDuration lacks.
func parseSince(text string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(text, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 || n > 7 {
			return 0, fmt.Errorf("%q is not between 0 and 7 days", text)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(text)
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func run(ctx context.Context, opts options, stdout, stderr io.Writer) (int, error) {
	cfg, err := config.Load(opts.config)
	if err != nil {
		return 2, err
	}
	envName, env, err := cfg.Env(opts.env)
	if err != nil {
		return 2, err
	}
	if opts.command == "map" {
		m, err := buildMap(cfg)
		if err != nil {
			return 2, err
		}
		if opts.json {
			return 0, printJSON(stdout, m)
		}
		_, err = fmt.Fprintln(stdout, topology.Render(m))
		return 0, err
	}
	scope := source.Scope{
		Env: envName, Environment: env, Config: cfg,
		Window: report.Window{Start: opts.at.Add(-opts.since), End: opts.at},
		Client: query.New(func(line string) { _, _ = fmt.Fprintln(stderr, line) }),
		Google: query.Google(), GitHub: query.GitHub(),
		Cache: &source.Cache{},
	}
	// The repository map is optional: it adds facts to component pages and lets check and suggest
	// compare declarations with the repository.
	var m *topology.Map
	if cfg.Topology != nil && opts.command != "changes" && opts.command != "explain" {
		if m, err = buildMap(cfg); err != nil {
			return 2, err
		}
	}
	out := output{stdout: stdout, json: opts.json}
	switch opts.command {
	case "check":
		return drift.Check(ctx, scope, m, stdout)
	case "suggest":
		return drift.Suggest(ctx, scope, m, stdout)
	case "component":
		v, err := component.Build(ctx, scope, m, opts.ref)
		if err != nil {
			return 2, err
		}
		return out.emit(v, component.Render(v), v.Reports)
	case "changes":
		r := source.Delivery(ctx, scope)
		reports := []report.Report{r}
		lines := append([]string{"# Changes — " + envName}, report.RenderChanges(r.Changes)...)
		data := map[string]any{"changes": r.Changes, "findings": r.Findings, "problems": r.Problems}
		return out.emit(data, strings.Join(append(lines, report.RenderBlindSpots(reports)...), "\n"), reports)
	case "explain":
		e := explain.Explain(ctx, scope, opts.ref)
		reports := e.Reports()
		return out.emit(e, strings.Join(append(e.Lines, report.RenderBlindSpots(reports)...), "\n"), reports)
	}
	snap := snapshot.Build(ctx, scope, m, snapshot.Options{SLO: opts.slo})
	return out.emit(snap, report.RenderSnapshot(snap), snap.Sources)
}

type output struct {
	stdout io.Writer
	json   bool
}

// emit prints data as JSON or text as Markdown, and exits 1 when any source could not be read in full.
func (o output) emit(data any, text string, reports []report.Report) (int, error) {
	var err error
	if o.json {
		err = printJSON(o.stdout, data)
	} else {
		_, err = fmt.Fprintln(o.stdout, text)
	}
	return exitFor(reports, err)
}

func exitFor(reports []report.Report, err error) (int, error) {
	if err != nil {
		return 1, err
	}
	for _, r := range reports {
		if r.Status != query.OK {
			return 1, nil
		}
	}
	return 0, nil
}

// buildMap reads the repository anchors named in the config's topology section.
func buildMap(cfg *config.Config) (*topology.Map, error) {
	if cfg.Topology == nil {
		return nil, errors.New("the config has no topology section")
	}
	root := filepath.Join(cfg.Dir, cfg.Topology.Root)
	return topology.Build(*cfg.Topology, root, time.Now())
}

func main() {
	opts, err := parse(os.Args[1:])
	if err == nil && (opts.command == "version" || opts.command == "--version") {
		fmt.Println("vitals " + versionString())
		return
	}
	if err != nil {
		if msg := strings.TrimPrefix(err.Error(), errUsage.Error()+": "); msg != errUsage.Error() {
			fmt.Fprintln(os.Stderr, msg)
			fmt.Fprintln(os.Stderr)
		}
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code, err := run(ctx, opts, os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}
