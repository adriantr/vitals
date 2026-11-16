package source

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
)

// Cloud Logging allows about 60 entries:list reads a minute per project, so samples queue behind
// this many in flight rather than spending the minute in one burst.
const logReadsInFlight = 4

// LogEntry is the part of a Cloud Logging entry a sample needs.
type LogEntry struct {
	Timestamp    string          `json:"timestamp"`
	Severity     string          `json:"severity"`
	TextPayload  string          `json:"textPayload"`
	JSONPayload  json.RawMessage `json:"jsonPayload"`
	ProtoPayload *struct {
		MethodName string `json:"methodName"`
		Status     *struct {
			Message string `json:"message"`
		} `json:"status"`
	} `json:"protoPayload"`
}

// Sample turns an entry into one line: text, else the JSON payload's message field, else the
// whole JSON payload, else the audit log's method and status.
func (e LogEntry) Sample() report.LogSample {
	msg := e.TextPayload
	if msg == "" && len(e.JSONPayload) > 0 {
		var fields map[string]any
		if json.Unmarshal(e.JSONPayload, &fields) == nil {
			for _, key := range []string{"message", "msg", "error", "err"} {
				if text, ok := fields[key].(string); ok && text != "" {
					msg = text
					break
				}
			}
		}
		if msg == "" {
			msg = string(e.JSONPayload)
		}
	}
	if msg == "" && e.ProtoPayload != nil {
		msg = e.ProtoPayload.MethodName
		if e.ProtoPayload.Status != nil && e.ProtoPayload.Status.Message != "" {
			msg += ": " + e.ProtoPayload.Status.Message
		}
	}
	return report.LogSample{At: at(e.Timestamp), Severity: e.Severity, Message: report.FirstLine(strings.Join(strings.Fields(firstLine(msg)), " "), report.MaxSampleRunes)}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}

// WindowFilter limits a Logging filter to the window. The caller's filter is parenthesised so an
// OR in it cannot escape the time range.
func WindowFilter(filter string, w report.Window) string {
	return fmt.Sprintf(`(%s) AND timestamp>=%q AND timestamp<=%q`, filter, iso(w.Start), iso(w.End))
}

// LogLines reads the newest `limit` entries matching filter in the window, newest first: one
// entries:list call.
func LogLines(ctx context.Context, s Scope, t *report.Tracker, title, filter string, limit int) ([]report.LogSample, bool) {
	body := map[string]any{
		"resourceNames": []string{"projects/" + string(s.Environment.GCP.Project)},
		"filter":        WindowFilter(filter, s.Window),
		"orderBy":       "timestamp desc",
		"pageSize":      limit,
	}
	var page struct {
		Entries []LogEntry `json:"entries"`
	}
	if !t.JSON(ctx, title, endpoint("https://logging.googleapis.com/v2/entries:list", nil), body, s.Google, &page) {
		return nil, false
	}
	out := make([]report.LogSample, 0, min(limit, len(page.Entries)))
	for _, e := range page.Entries[:min(limit, len(page.Entries))] {
		out = append(out, e.Sample())
	}
	return out, true
}

// sampleLogs reads the log lines of every breached job that has a logs block, at most
// logReadsInFlight at a time. Jobs that share an expanded filter share one read.
func sampleLogs(ctx context.Context, s Scope, t *report.Tracker, jobs []signalJob) map[string][]report.LogSample {
	limits := map[string]int{}
	for _, j := range jobs {
		if j.logFilter != "" {
			limits[j.logFilter] = max(limits[j.logFilter], j.sig.Logs.SampleSize())
		}
	}
	filters := slices.Sorted(maps.Keys(limits))
	slots := make(chan struct{}, logReadsInFlight)
	out := map[string][]report.LogSample{}
	for i, lines := range parallel(filters, func(f string) []report.LogSample {
		slots <- struct{}{}
		defer func() { <-slots }()
		lines, _ := LogLines(ctx, s, t, "log sample", f, limits[f])
		return lines
	}) {
		out[filters[i]] = lines
	}
	return out
}

// LogMetricNames lists the project's user-defined log-based metrics.
func LogMetricNames(ctx context.Context, s Scope, t *report.Tracker) (map[string]bool, bool) {
	base := fmt.Sprintf("https://logging.googleapis.com/v2/projects/%s/metrics", s.Environment.GCP.Project)
	title := "log-based metrics"
	failed := false
	metrics := paged(t, title, func(token string) ([]string, string, bool) {
		params := url.Values{"pageSize": {"500"}}
		if token != "" {
			params.Set("pageToken", token)
		}
		var page struct {
			Metrics []struct {
				Name string `json:"name"`
			} `json:"metrics"`
			NextPageToken string `json:"nextPageToken"`
		}
		ok := t.JSON(ctx, title, endpoint(base, params), nil, s.Google, &page)
		failed = failed || !ok
		names := make([]string, 0, len(page.Metrics))
		for _, m := range page.Metrics {
			names = append(names, m.Name)
		}
		return names, page.NextPageToken, ok
	})
	out := map[string]bool{}
	for _, m := range metrics {
		out[m] = true
	}
	return out, !failed
}

var logMetricRef = regexp.MustCompile(`logging_googleapis_com:user_([A-Za-z0-9_]+)`)

// LogMetricsNamed lists the user-defined log-based metrics a PromQL query reads. Cloud Monitoring
// turns every character of the metric name other than letters and digits into "_", so the names
// returned are in that form.
func LogMetricsNamed(promql string) []string {
	var out []string
	for _, m := range logMetricRef.FindAllStringSubmatch(promql, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

var nonPromQLName = regexp.MustCompile(`[^A-Za-z0-9_]`)

// PromQLName is how Cloud Monitoring PromQL spells a log-based metric's name.
func PromQLName(metric string) string {
	return nonPromQLName.ReplaceAllString(metric, "_")
}

// LogPattern is one group of similar log messages and how often it occurred.
type LogPattern struct {
	Pattern string
	Count   int64
}

// LogPatterns groups the window's messages from a Log Analytics view, through its linked BigQuery
// dataset, after replacing numbers and ids with "#". condition is SQL over the view's columns.
func LogPatterns(ctx context.Context, s Scope, t *report.Tracker, table, condition string, top int) ([]LogPattern, bool) {
	sql := fmt.Sprintf("SELECT REGEXP_REPLACE(LEFT(COALESCE(text_payload, JSON_VALUE(json_payload.message), TO_JSON_STRING(json_payload), ''), 300), r'[0-9a-fA-F]{8,}|[0-9a-fA-F-]{36}|\\d+', '#') AS pattern, COUNT(*) AS n "+
		"FROM `%s` WHERE timestamp BETWEEN TIMESTAMP(%q) AND TIMESTAMP(%q) AND (%s) GROUP BY pattern ORDER BY n DESC LIMIT %d",
		table, iso(s.Window.Start), iso(s.Window.End), condition, top)
	body := map[string]any{"query": sql, "useLegacySql": false, "timeoutMs": 60000, "maxResults": top}
	var resp struct {
		JobComplete bool `json:"jobComplete"`
		Rows        []struct {
			F []struct {
				V *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	u := endpoint(fmt.Sprintf("https://bigquery.googleapis.com/bigquery/v2/projects/%s/queries", s.Environment.GCP.Project), nil)
	if !t.JSON(ctx, "log patterns", u, body, s.Google, &resp) {
		return nil, false
	}
	if !resp.JobComplete {
		t.Fail(query.Failed, "log patterns: the Log Analytics query did not finish within 60s")
		return nil, false
	}
	out := make([]LogPattern, 0, len(resp.Rows))
	for _, r := range resp.Rows {
		if len(r.F) != 2 || r.F[0].V == nil || r.F[1].V == nil {
			continue
		}
		n, _ := strconv.ParseInt(*r.F[1].V, 10, 64)
		out = append(out, LogPattern{Pattern: *r.F[0].V, Count: n})
	}
	return out, true
}

// ServiceLogCondition is the analytics-view SQL that selects a Cloud Run service's warnings and
// errors; a Logging filter has no SQL translation, so patterns cover the service, not the filter.
func ServiceLogCondition(service string) string {
	return fmt.Sprintf("resource.type = 'cloud_run_revision' AND JSON_VALUE(resource.labels.service_name) = %s AND severity_number >= 400", sqlString(service))
}

func sqlString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
