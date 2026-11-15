package source

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
)

const (
	maxNewErrors = 10
	topErrors    = 3
)

// ErrorGroupID names an Error Reporting group.
type ErrorGroupID string

type ErrorGroupStats struct {
	Location config.ErrorLocation `json:"location"`
	Group    struct {
		GroupID          ErrorGroupID `json:"groupId"`
		ResolutionStatus string       `json:"resolutionStatus"`
	} `json:"group"`
	Count          string `json:"count"`
	FirstSeenTime  string `json:"firstSeenTime"`
	LastSeenTime   string `json:"lastSeenTime"`
	Representative struct {
		Message        string `json:"message"`
		ServiceContext struct {
			Service string `json:"service"`
		} `json:"serviceContext"`
	} `json:"representative"`
}

func (g ErrorGroupStats) count() int {
	n, _ := strconv.Atoi(g.Count)
	return n
}

func (g ErrorGroupStats) describe() string {
	return fmt.Sprintf("%s× in %s: %s", g.Count, g.Representative.ServiceContext.Service, report.FirstLine(g.Representative.Message, 160))
}

type ErrorPeriod struct {
	API   string
	Label string
}

// ErrorPeriodFor picks Error Reporting's smallest fixed period that covers the window; it takes no interval.
func ErrorPeriodFor(w report.Window) ErrorPeriod {
	switch hours := w.Hours(); {
	case hours <= 1:
		return ErrorPeriod{"PERIOD_1_HOUR", "1h"}
	case hours <= 6:
		return ErrorPeriod{"PERIOD_6_HOURS", "6h"}
	case hours <= 24:
		return ErrorPeriod{"PERIOD_1_DAY", "1d"}
	default:
		return ErrorPeriod{"PERIOD_1_WEEK", "1w"}
	}
}

// GroupStats reads error groups from every configured Error Reporting location, most frequent first.
func GroupStats(ctx context.Context, s Scope, t *report.Tracker, groupID ErrorGroupID) []ErrorGroupStats {
	perLocation := parallel(s.Environment.GCP.ErrorLocations, func(loc config.ErrorLocation) []ErrorGroupStats {
		return groupStatsIn(ctx, s, t, loc, groupID)
	})
	all := slices.Concat(perLocation...)
	slices.SortStableFunc(all, func(a, b ErrorGroupStats) int { return cmp.Compare(b.count(), a.count()) })
	return all
}

func groupStatsIn(ctx context.Context, s Scope, t *report.Tracker, loc config.ErrorLocation, groupID ErrorGroupID) []ErrorGroupStats {
	u := ErrorsURL(s.Environment.GCP.Project, loc, "groupStats")
	title := fmt.Sprintf("error groups (%s)", loc)
	return paged(t, title, func(token string) ([]ErrorGroupStats, string, bool) {
		params := pageParams(token, url.Values{"timeRange.period": {ErrorPeriodFor(s.Window).API}, "order": {"COUNT_DESC"}})
		params.Set("pageSize", "100")
		if groupID != "" {
			params.Set("groupId", string(groupID))
		}
		var page struct {
			Stats         []ErrorGroupStats `json:"errorGroupStats"`
			NextPageToken string            `json:"nextPageToken"`
		}
		ok := t.JSON(ctx, title, endpoint(u, params), nil, s.Google, &page)
		for i := range page.Stats {
			page.Stats[i].Location = loc
		}
		return page.Stats, page.NextPageToken, ok
	})
}

// ErrorsURL is an Error Reporting endpoint, such as groupStats or events, in one location.
func ErrorsURL(project config.ProjectID, loc config.ErrorLocation, resource string) string {
	return fmt.Sprintf("https://clouderrorreporting.googleapis.com/v1beta1/projects/%s/locations/%s/%s", project, loc, resource)
}

func Errors(ctx context.Context, s Scope) report.Report {
	t := report.NewTracker("errors", s.Client)
	if time.Since(s.Window.End) > 5*time.Minute {
		t.Fail(query.Skipped, "Error Reporting answers only windows that end now; errors for a past window are unknown")
		return t.Report(0, "error groups", nil, nil, nil)
	}
	period := ErrorPeriodFor(s.Window)
	var groups []ErrorGroupStats
	for _, g := range GroupStats(ctx, s, t, "") {
		if g.Group.ResolutionStatus != "MUTED" {
			groups = append(groups, g)
		}
	}
	var findings []report.Finding
	fresh := 0
	for _, g := range groups {
		if at(g.FirstSeenTime).Before(s.Window.Start) {
			continue
		}
		if fresh++; fresh > maxNewErrors {
			continue
		}
		findings = append(findings, report.Finding{
			Level: report.Amber, Source: "errors",
			Title:  "New error in " + g.Representative.ServiceContext.Service,
			Detail: g.describe(), Since: parseTime(g.FirstSeenTime), Ref: report.NewRef(report.RefError, string(g.Group.GroupID)),
			Service: g.Representative.ServiceContext.Service,
		})
	}
	var notes []string
	if fresh > maxNewErrors {
		notes = append(notes, fmt.Sprintf("%d more new error groups not listed", fresh-maxNewErrors))
	}
	for _, g := range groups[:min(topErrors, len(groups))] {
		notes = append(notes, fmt.Sprintf("top error (%s): %s [error:%s]", period.Label, g.describe(), g.Group.GroupID))
	}
	return t.Report(len(groups), "error groups seen in the last "+period.Label, findings, nil, notes)
}
