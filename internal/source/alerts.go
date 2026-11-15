package source

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
)

const maxPages = 5

var noiseLabels = map[string]bool{
	"project_id": true, "location": true, "configuration_name": true, "revision_name": true, "instance_id": true,
}

type Policy struct {
	Name          string `json:"name"`
	DisplayName   string `json:"displayName"`
	Enabled       *bool  `json:"enabled"`
	Documentation struct {
		Content string `json:"content"`
	} `json:"documentation"`
	Conditions []map[string]any `json:"conditions"`
}

func (p Policy) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

type Alert struct {
	State     string `json:"state"`
	OpenTime  string `json:"openTime"`
	CloseTime string `json:"closeTime"`
	Resource  struct {
		Labels map[string]string `json:"labels"`
	} `json:"resource"`
	Metric struct {
		Labels map[string]string `json:"labels"`
	} `json:"metric"`
	Log struct {
		ExtractedLabels map[string]string `json:"extractedLabels"`
	} `json:"log"`
	Policy struct {
		DisplayName string `json:"displayName"`
	} `json:"policy"`
}

// Labels names what fired in the alert's own terms: the service, subscription, route or log fields.
func (a Alert) Labels() string {
	merged := map[string]string{}
	for _, labels := range []map[string]string{a.Resource.Labels, a.Metric.Labels, a.Log.ExtractedLabels} {
		maps.Copy(merged, labels)
	}
	var parts []string
	for _, key := range slices.Sorted(maps.Keys(merged)) {
		if noiseLabels[key] {
			continue
		}
		parts = append(parts, key+"="+report.FirstLine(merged[key], 60))
	}
	return strings.Join(parts, " ")
}

func ListPolicies(ctx context.Context, s Scope, t *report.Tracker, project config.ProjectID) []Policy {
	title := string(project) + " alert policies"
	return paged(t, title, func(token string) ([]Policy, string, bool) {
		var page struct {
			AlertPolicies []Policy `json:"alertPolicies"`
			NextPageToken string   `json:"nextPageToken"`
		}
		u := endpoint(fmt.Sprintf("https://monitoring.googleapis.com/v3/projects/%s/alertPolicies", project), pageParams(token, nil))
		ok := t.JSON(ctx, title, u, nil, s.Google, &page)
		return page.AlertPolicies, page.NextPageToken, ok
	})
}

func ListAlerts(ctx context.Context, s Scope, t *report.Tracker, project config.ProjectID, filter string) []Alert {
	title := string(project) + " alerts"
	return paged(t, title, func(token string) ([]Alert, string, bool) {
		var page struct {
			Alerts        []Alert `json:"alerts"`
			NextPageToken string  `json:"nextPageToken"`
		}
		params := pageParams(token, url.Values{"filter": {filter}, "orderBy": {"open_time desc"}})
		u := endpoint(fmt.Sprintf("https://monitoring.googleapis.com/v3/projects/%s/alerts", project), params)
		ok := t.JSON(ctx, title, u, nil, s.Google, &page)
		return page.Alerts, page.NextPageToken, ok
	})
}

// AlertWindowFilter selects alerts open at any point of the window.
func AlertWindowFilter(w report.Window) string {
	return fmt.Sprintf(`open_time <= "%s" AND (state = "OPEN" OR close_time >= "%s")`, iso(w.End), iso(w.Start))
}

func distinctLabels(alerts []Alert) string {
	var distinct []string
	for _, a := range alerts {
		if l := a.Labels(); l != "" && !slices.Contains(distinct, l) {
			distinct = append(distinct, l)
		}
	}
	if len(distinct) == 0 {
		return "no labels"
	}
	if len(distinct) > 3 {
		return fmt.Sprintf("%s; +%d more", strings.Join(distinct[:3], "; "), len(distinct)-3)
	}
	return strings.Join(distinct, "; ")
}

// serviceOf names the Cloud Run service when every alert of the policy fired on the same one.
func serviceOf(alerts []Alert) string {
	service := ""
	for _, a := range alerts {
		name := a.Resource.Labels["service_name"]
		for _, labels := range []map[string]string{a.Resource.Labels, a.Log.ExtractedLabels} {
			if name == "" {
				name = labels["pipeline_id"] // Cloud Deploy pipelines are named after their service
			}
		}
		if name == "" || (service != "" && name != service) {
			return ""
		}
		service = name
	}
	return service
}

func oldest(alerts []Alert) string {
	first := alerts[0].OpenTime
	for _, a := range alerts[1:] {
		if at(a.OpenTime).Before(at(first)) {
			first = a.OpenTime
		}
	}
	return first
}

// policyOutcome: red while an incident is open, amber when it keeps firing and closing, else a note.
func policyOutcome(s Scope, policy string, alerts []Alert) (*report.Finding, string) {
	var open, fired, closed []Alert
	for _, a := range alerts {
		if at(a.OpenTime).After(s.Window.End) {
			continue
		}
		if a.State != "OPEN" && !at(a.CloseTime).Before(s.Window.Start) && !at(a.CloseTime).After(s.Window.End) {
			closed = append(closed, a)
		}
		// Open at the end of the window: still open, or closed only after it.
		if a.State == "OPEN" || at(a.CloseTime).After(s.Window.End) {
			open = append(open, a)
		}
		if !at(a.OpenTime).Before(s.Window.Start) {
			fired = append(fired, a)
		}
	}
	finding := report.Finding{Source: "alerts", Title: policy, Ref: report.NewRef(report.RefPolicy, policy), Service: serviceOf(alerts)}
	switch {
	case len(open) > 0:
		firedText := fmt.Sprintf("fired %d× in window", len(fired))
		if len(fired) == 0 {
			firedText = "opened before the window"
		}
		finding.Level, finding.Since = report.Red, parseTime(oldest(open))
		finding.Chronic = finding.Since != nil && s.Window.End.Sub(*finding.Since) > s.Config.Thresholds.ChronicAlertAge
		finding.Detail = fmt.Sprintf("%d open, %s, on %s", len(open), firedText, distinctLabels(open))
		return &finding, ""
	case len(fired) > 0 && len(fired) >= s.Config.Thresholds.FlappingFires:
		finding.Level, finding.Since = report.Amber, parseTime(oldest(fired))
		finding.Detail = fmt.Sprintf("flapping: fired %d× in window, all closed now, on %s", len(fired), distinctLabels(fired))
		return &finding, ""
	case len(fired) > 0:
		return nil, fmt.Sprintf("resolved: %s (fired %d×, last %s)", policy, len(fired), fired[0].OpenTime)
	case len(closed) > 0:
		return nil, fmt.Sprintf("resolved: %s (open since %s, closed in window, on %s)", policy, oldest(closed), distinctLabels(closed))
	default:
		return nil, ""
	}
}

const maxListed = 10

// ownershipNotes flags claims naming policies that do not exist, and enabled policies nobody claims
// (they get an owner only when their alerts name a service).
func ownershipNotes(s Scope, answers []policyAnswer, complete bool) []string {
	live := map[string]bool{}
	var unclaimed []string
	for _, a := range answers {
		for _, p := range a.policies {
			live[p.DisplayName] = true
			if p.IsEnabled() && s.Config.Owner(config.ClaimAlert, p.DisplayName) == "" {
				unclaimed = append(unclaimed, p.DisplayName)
			}
		}
	}
	// A project that failed to list would make its claimed policies look missing.
	if len(live) == 0 || !complete {
		return nil
	}
	var notes []string
	for _, name := range slices.Sorted(maps.Keys(s.Config.Claimed(config.ClaimAlert))) {
		if !live[name] {
			notes = append(notes, fmt.Sprintf("ownership: owners.%s claims alert policy %q, which does not exist", s.Config.Claimed(config.ClaimAlert)[name], name))
		}
	}
	if len(unclaimed) > 0 {
		slices.Sort(unclaimed)
		shown := unclaimed[:min(maxListed, len(unclaimed))]
		more := ""
		if len(unclaimed) > maxListed {
			more = fmt.Sprintf(" (+%d more)", len(unclaimed)-maxListed)
		}
		notes = append(notes, fmt.Sprintf("ownership: %d enabled policies have no owner in the config: %s%s", len(unclaimed), strings.Join(shown, "; "), more))
	}
	return notes
}

type policyAnswer struct {
	policies []Policy
	alerts   []Alert
}

func Alerts(ctx context.Context, s Scope) report.Report {
	t := report.NewTracker("alerts", s.Client)
	filter := AlertWindowFilter(s.Window)
	answers := parallel(s.Environment.GCP.AlertProjects, func(project config.ProjectID) policyAnswer {
		return policyAnswer{ListPolicies(ctx, s, t, project), ListAlerts(ctx, s, t, project, filter)}
	})
	enabled := 0
	byPolicy := map[string][]Alert{}
	for _, a := range answers {
		for _, p := range a.policies {
			if p.IsEnabled() {
				enabled++
			}
		}
		for _, alert := range a.alerts {
			byPolicy[alert.Policy.DisplayName] = append(byPolicy[alert.Policy.DisplayName], alert)
		}
	}
	var findings []report.Finding
	var notes []string
	for _, policy := range slices.Sorted(maps.Keys(byPolicy)) {
		finding, note := policyOutcome(s, policy, byPolicy[policy])
		if finding != nil {
			findings = append(findings, *finding)
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	notes = append(notes, ownershipNotes(s, answers, t.Status() == query.OK)...)
	return t.Report(enabled, "enabled alert policies", findings, nil, notes)
}
