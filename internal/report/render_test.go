package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampledFinding() Finding {
	return Finding{
		Level: Red, Source: "signals", Title: "payment-timeouts", Owner: "checkout", OwnerIsService: true,
		Detail: "payment-timeouts is 42, above the red threshold 20",
		Ref:    NewRef(RefSignal, "checkout/payment-timeouts"),
		Samples: []LogSample{
			{At: time.Date(2026, 3, 4, 11, 58, 1, 0, time.UTC), Severity: "ERROR", Message: "payment provider timed out after 10s"},
			{At: time.Date(2026, 3, 4, 11, 57, 40, 0, time.UTC), Message: "payment provider timed out after 10s"},
		},
	}
}

func TestSnapshotRendersSamples(t *testing.T) {
	out := RenderSnapshot(Snapshot{Env: "prod", Overall: Red, Findings: []Finding{sampledFinding()}})
	want := strings.Join([]string{
		"- **payment-timeouts** [signals · checkout] — payment-timeouts is 42, above the red threshold 20 · `vitals component checkout` · ref `signal:checkout/payment-timeouts`",
		"  - `2026-03-04 11:58:01Z ERROR` payment provider timed out after 10s",
		"  - `2026-03-04 11:57:40Z DEFAULT` payment provider timed out after 10s",
	}, "\n")
	if !strings.Contains(out, want) {
		t.Errorf("snapshot does not render the samples under the finding:\n%s", out)
	}
}

func TestSamplesInJSON(t *testing.T) {
	raw, err := json.Marshal(sampledFinding())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"samples":[{"timestamp":"2026-03-04T11:58:01Z","severity":"ERROR","message":"payment provider timed out after 10s"}`) {
		t.Errorf("JSON = %s", raw)
	}
	raw, _ = json.Marshal(Finding{Level: Amber})
	if strings.Contains(string(raw), "samples") {
		t.Errorf("a finding without samples must leave the key out: %s", raw)
	}
}
