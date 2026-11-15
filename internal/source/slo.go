package source

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/report"
)

// Fast burns surface through their alert policies, which keeps this source to two reads per SLO
// under the Monitoring API's per-user request quota.
const (
	slowBurnLookback = "360m"
	latestSpan       = 15 * time.Minute
)

type SLO struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"displayName"`
	Goal        float64 `json:"goal"`
}

// Ref turns projects/<n>/services/<service>/serviceLevelObjectives/<slo> into <service>/<slo>.
func (o SLO) Ref() string { return SLORef(o.Name) }

func SLORef(name string) string {
	_, rest, _ := strings.Cut(name, "/services/")
	return strings.Replace(rest, "/serviceLevelObjectives/", "/", 1)
}

func ListSLOs(ctx context.Context, s Scope, t *report.Tracker) []SLO {
	return paged(t, "SLOs", func(token string) ([]SLO, string, bool) {
		var page struct {
			SLOs          []SLO  `json:"serviceLevelObjectives"`
			NextPageToken string `json:"nextPageToken"`
		}
		u := endpoint(fmt.Sprintf("https://monitoring.googleapis.com/v3/projects/%s/services/-/serviceLevelObjectives", s.Environment.GCP.Project), pageParams(token, nil))
		ok := t.JSON(ctx, "SLOs", u, nil, s.Google, &page)
		return page.SLOs, page.NextPageToken, ok
	})
}

func latest(ctx context.Context, s Scope, t *report.Tracker, title, filter string) *Reading {
	w := report.Window{Start: s.Window.End.Add(-latestSpan), End: s.Window.End}
	points, _ := Series(ctx, s, t, title, filter, w, nil)
	if len(points) == 0 {
		return nil
	}
	return &points[len(points)-1]
}

func sloOutcome(ctx context.Context, s Scope, t *report.Tracker, slo SLO) (*report.Finding, string) {
	ref := slo.Ref()
	th := s.Config.Thresholds
	slow := latest(ctx, s, t, ref+" slow burn", fmt.Sprintf("select_slo_burn_rate(%q, %q)", slo.Name, slowBurnLookback))
	// The budget is read only for an SLO that is burning: a spent budget alone is history, not health.
	if slow == nil || slow.Value < th.SLOSlowBurn {
		return nil, ""
	}
	budget := latest(ctx, s, t, ref+" budget", fmt.Sprintf("select_slo_budget_fraction(%q)", slo.Name))

	title := slo.DisplayName
	if title == "" {
		title = ref
	}
	budgetText := "budget unknown"
	if budget != nil {
		budgetText = fmt.Sprintf("%d%% of the error budget left", int(math.Round(budget.Value*100)))
	}
	service, _, _ := strings.Cut(ref, "/")
	level := report.Amber
	if slow.Value >= th.SLOSevereBurn {
		level = report.Red
	}
	return &report.Finding{
		Level: level, Source: "slo", Title: title, Ref: report.NewRef(report.RefSLO, ref), Service: service,
		Detail: fmt.Sprintf("burning %.1f× the sustainable rate over 6h; %s (goal %g)", slow.Value, budgetText, slo.Goal),
	}, ""
}

// SLOServiceIDs are the Monitoring services whose SLOs count: those claimed in the config, and the
// declared Cloud Run services, whose SLOs Cloud Monitoring keys by the service name.
func SLOServiceIDs(cfg *config.Config) []string {
	ids := map[string]bool{}
	for id := range cfg.Claimed(config.ClaimSLOService) {
		ids[id] = true
	}
	for name, svc := range cfg.Services {
		if svc.ServiceKind() == config.KindCloudRun {
			ids[name] = true
		}
	}
	return slices.Sorted(maps.Keys(ids))
}

func SLOs(ctx context.Context, s Scope) report.Report {
	return SLOsFor(ctx, s, SLOServiceIDs(s.Config))
}

// SLOsFor evaluates the SLOs of the given Monitoring services.
func SLOsFor(ctx context.Context, s Scope, services []string) report.Report {
	t := report.NewTracker("slo", s.Client)
	var slos []SLO
	for _, o := range ListSLOs(ctx, s, t) {
		service, _, _ := strings.Cut(o.Ref(), "/")
		if slices.Contains(services, service) {
			slos = append(slos, o)
		}
	}
	type outcome struct {
		finding *report.Finding
		note    string
	}
	outcomes := parallel(slos, func(slo SLO) outcome {
		f, n := sloOutcome(ctx, s, t, slo)
		return outcome{f, n}
	})
	var findings []report.Finding
	var notes []string
	for _, o := range outcomes {
		if o.finding != nil {
			findings = append(findings, *o.finding)
		}
		if o.note != "" {
			notes = append(notes, o.note)
		}
	}
	return t.Report(len(slos), "SLOs", findings, nil, notes)
}
