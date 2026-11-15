package source

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
)

// Services only: a scheduler that rewrites a Cloud Run job on every run would bury the real deploys.
var deployMethods = []string{"ReplaceService", "CreateService"}

type WorkflowRun struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	CreatedAt  string `json:"created_at"`
	Title      string `json:"display_title"`
	HTMLURL    string `json:"html_url"`
}

func (r WorkflowRun) Outcome() string {
	if r.Status == "completed" {
		return r.Conclusion
	}
	return r.Status
}

// BranchRuns lists the latest runs of one workflow file on the configured branch, newest first.
func BranchRuns(ctx context.Context, s Scope, t *report.Tracker, workflow string) []WorkflowRun {
	gh := s.Environment.GitHub
	u := endpoint(
		fmt.Sprintf("https://api.github.com/repos/%s/actions/workflows/%s/runs", gh.Repo, url.PathEscape(workflow)),
		url.Values{"branch": {gh.Branch}, "per_page": {"100"}, "created": {"<=" + iso(s.Window.End)}},
	)
	var page struct {
		Runs []WorkflowRun `json:"workflow_runs"`
	}
	title := workflow + " runs on " + gh.Branch
	t.JSON(ctx, title, u, nil, s.GitHub, &page)
	// A full page whose oldest run is still inside the window means older failures were not read.
	if n := len(page.Runs); n == 100 && !at(page.Runs[n-1].CreatedAt).Before(s.Window.Start) {
		t.Fail(query.Failed, title+": more than 100 runs in the window; older ones are not shown")
	}
	return page.Runs
}

// failedRun is a completed run that did not succeed for a reason other than being cancelled or skipped.
func failedRun(r WorkflowRun) bool {
	switch r.Conclusion {
	case "failure", "timed_out", "startup_failure":
		return true
	default:
		return false
	}
}

func workflowFinding(ctx context.Context, s Scope, t *report.Tracker, workflow string) *report.Finding {
	var completed, inWindow, failed []WorkflowRun
	for _, run := range BranchRuns(ctx, s, t, workflow) {
		if run.Status != "completed" || at(run.CreatedAt).After(s.Window.End) {
			continue
		}
		completed = append(completed, run)
		if !at(run.CreatedAt).Before(s.Window.Start) {
			inWindow = append(inWindow, run)
			if failedRun(run) {
				failed = append(failed, run)
			}
		}
	}
	if len(failed) == 0 {
		return nil
	}
	latest := completed[0]
	level, tail := report.Amber, "recovered"
	if failedRun(latest) {
		level, tail = report.Red, latest.HTMLURL
	}
	return &report.Finding{
		Level: level, Source: "delivery", Owner: string(s.Environment.GitHub.Repo),
		Title:  fmt.Sprintf("Workflow %s on %s", workflow, s.Environment.GitHub.Branch),
		Detail: fmt.Sprintf("%d of %d runs failed in window; latest %s at %s: %s", len(failed), len(inWindow), latest.Conclusion, latest.CreatedAt, tail),
		Since:  parseTime(failed[len(failed)-1].CreatedAt), Ref: report.NewRef(report.RefWorkflow, workflow),
	}
}

func releases(ctx context.Context, s Scope, t *report.Tracker) []report.Change {
	var out []report.Change
	for _, r := range listReleases(ctx, s, t, s.Window.Start) {
		if at := parseTime(r.PublishedAt); at != nil && !at.Before(s.Window.Start) && !at.After(s.Window.End) {
			out = append(out, report.Change{At: *at, Kind: report.Release, What: r.TagName})
		}
	}
	return out
}

// Deploys groups Cloud Run service deploys from the admin-activity audit log, newest deploy per service.
func Deploys(ctx context.Context, s Scope, t *report.Tracker, project config.ProjectID) []report.Change {
	methods := make([]string, 0, len(deployMethods))
	for _, m := range deployMethods {
		methods = append(methods, fmt.Sprintf("%q", m))
	}
	body := map[string]any{
		"resourceNames": []string{"projects/" + string(project)},
		"filter": fmt.Sprintf(
			`logName="projects/%s/logs/cloudaudit.googleapis.com%%2Factivity" AND protoPayload.serviceName="run.googleapis.com" AND protoPayload.methodName:(%s) AND timestamp>="%s" AND timestamp<="%s"`,
			project, strings.Join(methods, " OR "), iso(s.Window.Start), iso(s.Window.End),
		),
		"orderBy":  "timestamp desc",
		"pageSize": 1000,
	}
	type entry struct {
		Timestamp    string `json:"timestamp"`
		ProtoPayload struct {
			ResourceName       string `json:"resourceName"`
			AuthenticationInfo struct {
				PrincipalEmail string `json:"principalEmail"`
			} `json:"authenticationInfo"`
		} `json:"protoPayload"`
	}
	title := string(project) + " Cloud Run deploys"
	entries := paged(t, title, func(token string) ([]entry, string, bool) {
		if token != "" {
			body["pageToken"] = token
		}
		var page struct {
			Entries       []entry `json:"entries"`
			NextPageToken string  `json:"nextPageToken"`
		}
		ok := t.JSON(ctx, title, endpoint("https://logging.googleapis.com/v2/entries:list", nil), body, s.Google, &page)
		return page.Entries, page.NextPageToken, ok
	})
	type group struct {
		latest report.Change
		count  int
	}
	var order []string
	groups := map[string]*group{}
	for _, e := range entries {
		at := parseTime(e.Timestamp)
		if at == nil {
			continue
		}
		parts := strings.Split(e.ProtoPayload.ResourceName, "/")
		name := parts[len(parts)-1]
		if g, ok := groups[name]; ok {
			g.count++
			continue
		}
		order = append(order, name)
		groups[name] = &group{latest: report.Change{At: *at, Kind: report.Deploy, What: name, By: e.ProtoPayload.AuthenticationInfo.PrincipalEmail}, count: 1}
	}
	out := make([]report.Change, 0, len(order))
	for _, name := range order {
		g := groups[name]
		if g.count > 1 {
			g.latest.What = fmt.Sprintf("%s (%d deploys)", name, g.count)
		}
		out = append(out, g.latest)
	}
	return out
}

func Delivery(ctx context.Context, s Scope) report.Report {
	t := report.NewTracker("delivery", s.Client)
	var findings []report.Finding
	var released, deployed []report.Change
	checked := 0
	tasks := []func(){func() { deployed = Deploys(ctx, s, t, s.Environment.GCP.Project) }}
	if gh := s.Environment.GitHub; gh != nil {
		checked = len(gh.Workflows)
		tasks = append(tasks,
			func() {
				for _, f := range parallel(gh.Workflows, func(w string) *report.Finding { return workflowFinding(ctx, s, t, w) }) {
					if f != nil {
						findings = append(findings, *f)
					}
				}
			},
			func() { released = releases(ctx, s, t) },
		)
	}
	Concurrently(tasks...)
	changes := slices.Concat(released, deployed)
	report.SortChanges(changes)
	return t.Report(checked, "deploy workflows", findings, changes, nil)
}
