package source

import (
	"context"
	"maps"
	"slices"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/report"
)

// Deployments reports declared Cloud Run services that Cloud Run does not have, or whose latest
// revision is not ready. It reads the current state, whatever the window.
func Deployments(ctx context.Context, s Scope) report.Report {
	t := report.NewTracker("deployments", s.Client)
	live := ListRunServices(ctx, s, t)
	var findings []report.Finding
	checked := 0
	for _, name := range slices.Sorted(maps.Keys(s.Config.Services)) {
		if s.Config.Services[name].ServiceKind() != config.KindCloudRun || len(live) == 0 {
			continue
		}
		checked++
		rs, ok := live[name]
		switch {
		case !ok:
			findings = append(findings, report.Finding{Level: report.Red, Source: "deployments", Owner: name, Title: "Not in Cloud Run",
				Detail: "declared as a Cloud Run service, but Cloud Run has no service by this name", Ref: report.NewRef(report.RefService, name)})
		case !rs.Ready:
			findings = append(findings, report.Finding{Level: report.Red, Source: "deployments", Owner: name, Title: "Latest revision not ready",
				Detail: "revision " + rs.Revision + " is not serving", Since: rs.UpdatedAt, Ref: report.NewRef(report.RefService, name)})
		}
	}
	return t.Report(checked, "Cloud Run services", findings, nil, nil)
}
