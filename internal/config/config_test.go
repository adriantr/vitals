package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adriantr/vitals/internal/topology"
)

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../examples/vitals.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	name, env, err := cfg.Env("")
	if err != nil {
		t.Fatal(err)
	}
	if name != "prod" || env.GCP.Project != "acme-prod" {
		t.Errorf("default env = %s/%s", name, env.GCP.Project)
	}
	if got := cfg.Owner(ClaimAlert, "Payment webhook failures"); got != "payments-provider" {
		t.Errorf("owner of the payment alert = %q", got)
	}
	if got := cfg.Owner(ClaimError, "ORDER-WORKER"); got != "order-worker" {
		t.Errorf("error claims must match without regard to case, got %q", got)
	}
}

func TestEnvDefaults(t *testing.T) {
	cfg := load(t, `
environments:
  prod:
    gcp: { project: p }
    github: { repo: o/r }
`)
	_, env, err := cfg.Env("prod")
	if err != nil {
		t.Fatal(err)
	}
	if env.GCP.Region != "europe-west1" || !slices.Equal(env.GCP.AlertProjects, []ProjectID{"p"}) ||
		!slices.Equal(env.GCP.ErrorLocations, []ErrorLocation{"global"}) || env.GitHub.Branch != "main" {
		t.Errorf("defaults not applied: %+v %+v", env.GCP, env.GitHub)
	}
	if cfg.Thresholds.ChronicAlertAge != 24*time.Hour || cfg.Thresholds.FlappingFires != 3 {
		t.Errorf("threshold defaults not applied: %+v", cfg.Thresholds)
	}
	if _, _, err := cfg.Env("staging"); err == nil {
		t.Error("an unknown environment must fail")
	}
}

func TestDefaultSignalsAreReplacedByOwnAttachment(t *testing.T) {
	cfg := load(t, `
environments: { prod: { gcp: { project: p } } }
signals:
  a: { promql: 'up{service="{service}"}' }
  b: { promql: 'up{service="{service}"}' }
defaults:
  cloud-run: { signals: [a, b] }
services:
  web: { signals: [{ use: b, amber: 0.5 }] }
`)
	got := cfg.AllSignals("web")
	if len(got) != 2 || got[0].Use != "a" || got[1].Use != "b" || got[1].Params["amber"] != "0.5" {
		t.Errorf("AllSignals = %+v", got)
	}
}

func TestInvalidConfigs(t *testing.T) {
	base := "environments: { prod: { gcp: { project: p } } }\n"
	cases := map[string]struct{ yaml, want string }{
		"misspelt key":        {base + "servises: {}\n", "servises"},
		"no environments":     {"services: {}\n", "no environments"},
		"undefined signal":    {base + "services: { web: { signals: [nope] } }\n", `undefined signal "nope"`},
		"missing parameter":   {base + "signals: { s: { promql: 'x{t=\"{topic}\"}' } }\nservices: { web: { signals: [s] } }\n", "{topic}"},
		"service on a system": {base + "signals: { s: { promql: 'x{s=\"{service}\"}' } }\nsystems: { db: { signals: [s] } }\n", "{service}"},
		"by with service":     {base + "signals: { s: { by: service_name, promql: 'x{s=\"{service}\"}' } }\n", "must not use {service}"},
		"bad noData":          {base + "signals: { s: { promql: x, noData: maybe } }\n", "noData"},
		"double claim":        {base + "services: { a: { alerts: [X] }, b: { alerts: [X] } }\n", "claimed by both"},
		"unknown dependency":  {base + "services: { a: { dependsOn: [ghost] } }\n", `undeclared "ghost"`},
		"unknown member":      {base + "systems: { s: { services: [ghost] } }\n", `undeclared service "ghost"`},
		"name clash":          {base + "services: { a: {} }\nsystems: { a: {} }\n", "both a service and a system"},
		"bad dead-letter re":  {base + "thresholds: { deadLetterMatch: '(' }\n", "deadLetterMatch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestExpand(t *testing.T) {
	sig := Signal{PromQL: `rate(x{service="{service}",topic="{topic}"}[{window}])`}
	got, err := sig.Expand(map[string]string{"service": "web", "topic": "orders", "window": "5m"})
	if err != nil || got != `rate(x{service="web",topic="orders"}[5m])` {
		t.Errorf("Expand = %q, %v", got, err)
	}
	if _, err := sig.Expand(map[string]string{"service": "web"}); err == nil {
		t.Error("Expand must fail on a missing placeholder")
	}
}

// TestConfigurationDocCoversEveryKey keeps docs/configuration.md in step with the config structs.
func TestConfigurationDocCoversEveryKey(t *testing.T) {
	raw, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	var keys []string
	collectKeys(reflect.TypeOf(Config{}), &keys, map[reflect.Type]bool{})
	collectKeys(reflect.TypeOf(topology.Config{}), &keys, map[reflect.Type]bool{})
	slices.Sort(keys)
	for _, key := range slices.Compact(keys) {
		if !regexp.MustCompile("`([A-Za-z]+\\.)*" + regexp.QuoteMeta(key) + "`").MatchString(doc) {
			t.Errorf("docs/configuration.md does not mention `%s`", key)
		}
	}
}

func collectKeys(t reflect.Type, keys *[]string, seen map[reflect.Type]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" || !f.IsExported() {
			continue
		}
		if name != "" {
			*keys = append(*keys, name)
		}
		collectKeys(f.Type, keys, seen)
	}
}

func load(t *testing.T, yaml string) *Config {
	t.Helper()
	cfg, err := Load(write(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func write(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vitals.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
