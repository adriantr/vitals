package source

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/report"
)

func exampleScope(t *testing.T) Scope {
	t.Helper()
	cfg, err := config.Load("../../examples/vitals.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	name, env, err := cfg.Env("")
	if err != nil {
		t.Fatal(err)
	}
	end := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	return Scope{Env: name, Environment: env, Config: cfg, Window: report.Window{Start: end.Add(-6 * time.Hour), End: end}}
}

func TestPlanExpandsLogFilter(t *testing.T) {
	s := exampleScope(t)
	jobs := planSignals(s, report.NewTracker("test", nil), []string{"checkout"})
	i := slices.IndexFunc(jobs, func(j signalJob) bool { return j.a.ref.Use == "payment-timeouts" })
	if i < 0 {
		t.Fatal("checkout has no payment-timeouts attachment in the example")
	}
	want := `resource.type="cloud_run_revision" AND resource.labels.service_name="checkout" AND textPayload:"payment provider timed out"`
	if jobs[i].err != nil || jobs[i].logFilter != want {
		t.Errorf("logFilter = %q, %v; want %q", jobs[i].logFilter, jobs[i].err, want)
	}
	for _, j := range jobs {
		if j.sig.Logs == nil && j.logFilter != "" {
			t.Errorf("%s has no logs block but a filter %q", j.a.ref.Use, j.logFilter)
		}
	}
}

func TestWindowFilter(t *testing.T) {
	w := report.Window{Start: time.Date(2026, 3, 4, 6, 0, 0, 0, time.UTC), End: time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)}
	got := WindowFilter(`severity>=ERROR OR textPayload:"x"`, w)
	want := `(severity>=ERROR OR textPayload:"x") AND timestamp>="2026-03-04T06:00:00Z" AND timestamp<="2026-03-04T12:00:00Z"`
	if got != want {
		t.Errorf("WindowFilter = %s\nwant %s", got, want)
	}
}

// A quiet run, or one whose breached signals have no logs block, must not touch Cloud Logging.
// The scope has no HTTP client, so any read would panic.
func TestNoSamplesWithoutLogsBlock(t *testing.T) {
	s := exampleScope(t)
	jobs := planSignals(s, report.NewTracker("test", nil), []string{"storefront"})
	findings := make([]report.Finding, len(jobs))
	if notes := attachSamples(context.Background(), s, jobs, findings); notes != nil {
		t.Errorf("notes = %v", notes)
	}
	if notes := attachSamples(context.Background(), s, nil, nil); notes != nil {
		t.Errorf("notes = %v", notes)
	}
	for _, f := range findings {
		if f.Samples != nil {
			t.Errorf("samples attached without a logs block: %+v", f)
		}
	}
}

func TestLogEntrySample(t *testing.T) {
	long := strings.Repeat("x", 300)
	cases := map[string]struct {
		entry string
		want  string
	}{
		"text, first line, spaces collapsed": {`{"textPayload":"  waiting   too long\n\tat Pool.get"}`, "waiting too long"},
		"json message":                       {`{"jsonPayload":{"message":"pool exhausted","n":3}}`, "pool exhausted"},
		"json msg":                           {`{"jsonPayload":{"msg":"retrying"}}`, "retrying"},
		"json without message":               {`{"jsonPayload":{"n":3}}`, `{"n":3}`},
		"audit":                              {`{"protoPayload":{"methodName":"Deploy","status":{"message":"denied"}}}`, "Deploy: denied"},
		"truncated":                          {`{"textPayload":"` + long + `"}`, strings.Repeat("x", report.MaxSampleRunes-1) + "…"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var e LogEntry
			if err := json.Unmarshal([]byte(tc.entry), &e); err != nil {
				t.Fatal(err)
			}
			if got := e.Sample().Message; got != tc.want {
				t.Errorf("message = %q, want %q", got, tc.want)
			}
		})
	}
	var e LogEntry
	_ = json.Unmarshal([]byte(`{"timestamp":"2026-03-04T11:58:01.123Z","severity":"ERROR","textPayload":"x"}`), &e)
	if got := e.Sample(); got.Severity != "ERROR" || !got.At.Equal(time.Date(2026, 3, 4, 11, 58, 1, 123e6, time.UTC)) {
		t.Errorf("sample = %+v", got)
	}
}

func TestLogMetricsNamed(t *testing.T) {
	got := LogMetricsNamed(`sum(logging_googleapis_com:user_payment_timeouts{a="b"}) / sum(logging_googleapis_com:user_payment_timeouts) + logging_googleapis_com:user_other + logging_googleapis_com:byte_count`)
	if !slices.Equal(got, []string{"payment_timeouts", "other"}) {
		t.Errorf("LogMetricsNamed = %q", got)
	}
	if got := PromQLName("checkout/payment-timeouts.v2"); got != "checkout_payment_timeouts_v2" {
		t.Errorf("PromQLName = %q", got)
	}
}

func TestServiceLogCondition(t *testing.T) {
	got := ServiceLogCondition(`it's`)
	if !strings.Contains(got, `= 'it\'s'`) {
		t.Errorf("service name not quoted for SQL: %s", got)
	}
}
