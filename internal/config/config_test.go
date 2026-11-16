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
		"misspelt key":            {base + "servises: {}\n", "servises"},
		"no environments":         {"services: {}\n", "no environments"},
		"undefined signal":        {base + "services: { web: { signals: [nope] } }\n", `undefined signal "nope"`},
		"missing parameter":       {base + "signals: { s: { promql: 'x{t=\"{topic}\"}' } }\nservices: { web: { signals: [s] } }\n", "{topic}"},
		"service on a system":     {base + "signals: { s: { promql: 'x{s=\"{service}\"}' } }\nsystems: { db: { signals: [s] } }\n", "{service}"},
		"by with service":         {base + "signals: { s: { by: service_name, promql: 'x{s=\"{service}\"}' } }\n", "must not use {service}"},
		"bad noData":              {base + "signals: { s: { promql: x, noData: maybe } }\n", "noData"},
		"double claim":            {base + "services: { a: { alerts: [X] }, b: { alerts: [X] } }\n", "claimed by both"},
		"unknown dependency":      {base + "services: { a: { dependsOn: [ghost] } }\n", `undeclared "ghost"`},
		"unknown member":          {base + "systems: { s: { services: [ghost] } }\n", `undeclared service "ghost"`},
		"name clash":              {base + "services: { a: {} }\nsystems: { a: {} }\n", "both a service and a system"},
		"bad dead-letter re":      {base + "thresholds: { deadLetterMatch: '(' }\n", "deadLetterMatch"},
		"logs without filter":     {base + "signals: { s: { promql: x, logs: { sample: 2 } } }\n", "logs.filter is required"},
		"logs sample zero":        {base + "signals: { s: { promql: x, logs: { filter: f, sample: 0 } } }\n", "logs.sample must be between 1 and 20"},
		"logs sample too big":     {base + "signals: { s: { promql: x, logs: { filter: f, sample: 21 } } }\n", "logs.sample must be between 1 and 20"},
		"logs with window":        {base + "signals: { s: { promql: x, logs: { filter: 'a AND {window}' } } }\n", "must not use {window}"},
		"logs unknown param":      {base + "signals: { s: { promql: x, logs: { filter: 'labels.k=\"{kind}\"' } } }\nservices: { web: { signals: [s] } }\n", "logs.filter has no value for {kind}"},
		"logs param is threshold": {base + "signals: { s: { promql: x, logs: { filter: 'x>{amber}' } } }\nservices: { web: { signals: [{ use: s, amber: 1 }] } }\n", "logs.filter has no value for {amber}"},
		"logs service on system":  {base + "signals: { s: { promql: x, logs: { filter: 'svc=\"{service}\"' } } }\nsystems: { db: { signals: [s] } }\n", "logs.filter has no value for {service}"},
		"logs unknown key":        {base + "signals: { s: { promql: x, logs: { filter: f, lines: 3 } } }\n", "lines"},
		"analytics placeholder":   {base + "logs: { analyticsView: '{team}.ds.v' }\n", "logs.analyticsView may only use"},
		"analytics shape":         {base + "logs: { analyticsView: 'just-a-view' }\n", "not project.dataset.view"},
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

func TestSignalLogs(t *testing.T) {
	cfg := load(t, `
environments: { prod: { gcp: { project: acme-prod } } }
logs: { analyticsView: '{project}.logs_{env}._AllLogs' }
signals:
  timeouts:
    promql: 'sum(logging_googleapis_com:user_timeouts{service_name="{service}"})'
    logs: { filter: 'resource.labels.service_name="{service}" AND labels.topic="{topic}" AND logName:"{project}"' }
  sampled:
    promql: x
    logs: { filter: f, sample: 20 }
services:
  web: { signals: [{ use: timeouts, topic: orders }, sampled] }
`)
	if got := cfg.Signals["timeouts"].Logs.SampleSize(); got != DefaultLogSample {
		t.Errorf("default sample = %d, want %d", got, DefaultLogSample)
	}
	if got := cfg.Signals["sampled"].Logs.SampleSize(); got != 20 {
		t.Errorf("sample = %d, want 20", got)
	}
	got, err := cfg.Signals["timeouts"].ExpandLogs(map[string]string{"service": "web", "topic": "orders", "project": "acme-prod", "env": "prod"})
	if want := `resource.labels.service_name="web" AND labels.topic="orders" AND logName:"acme-prod"`; err != nil || got != want {
		t.Errorf("ExpandLogs = %q, %v; want %q", got, err, want)
	}
	if _, err := cfg.Signals["timeouts"].ExpandLogs(map[string]string{"service": "web"}); err == nil {
		t.Error("ExpandLogs must fail on a missing placeholder")
	}
	if got, _ := (Signal{PromQL: "x"}).ExpandLogs(nil); got != "" {
		t.Errorf("a signal without logs expands to %q, want empty", got)
	}
	if got := cfg.AnalyticsTable("prod", "acme-prod"); got != "acme-prod.logs_prod._AllLogs" {
		t.Errorf("AnalyticsTable = %q", got)
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
