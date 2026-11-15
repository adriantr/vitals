package source

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
)

// PromSeries is one result series: its labels and value.
type PromSeries struct {
	Labels map[string]string
	Value  float64
}

// PromQL evaluates an instant query at the given time through Cloud Monitoring's Prometheus API.
// It returns one entry per series; an empty slice means the query matched nothing.
func PromQL(ctx context.Context, s Scope, t *report.Tracker, title, expr string, evalAt time.Time) ([]PromSeries, bool) {
	u := endpoint(fmt.Sprintf("https://monitoring.googleapis.com/v1/projects/%s/location/global/prometheus/api/v1/query", s.Environment.GCP.Project),
		url.Values{"query": {expr}, "time": {strconv.FormatInt(evalAt.Unix(), 10)}})
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string        `json:"resultType"`
			Result     jsonRawResult `json:"result"`
		} `json:"data"`
	}
	if !t.JSON(ctx, title, u, nil, s.Google, &resp) {
		return nil, false
	}
	if resp.Status != "success" {
		t.Fail(query.Failed, fmt.Sprintf("%s: PromQL answered status %q", title, resp.Status))
		return nil, false
	}
	return resp.Data.Result.values(resp.Data.ResultType), true
}

// jsonRawResult holds a vector ([{metric, value: [ts, "v"]}]) or a scalar ([ts, "v"]).
type jsonRawResult []any

func (r jsonRawResult) values(kind string) []PromSeries {
	parse := func(pair any) (float64, bool) {
		p, ok := pair.([]any)
		if !ok || len(p) != 2 {
			return 0, false
		}
		str, ok := p[1].(string)
		if !ok {
			return 0, false
		}
		v, err := strconv.ParseFloat(str, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return v, true
	}
	if kind == "scalar" {
		if v, ok := parse([]any(r)); ok {
			return []PromSeries{{Value: v}}
		}
		return nil
	}
	var out []PromSeries
	for _, item := range r {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		v, ok := parse(m["value"])
		if !ok {
			continue
		}
		labels := map[string]string{}
		if metric, ok := m["metric"].(map[string]any); ok {
			for k, val := range metric {
				if str, ok := val.(string); ok && k != "__name__" {
					labels[k] = str
				}
			}
		}
		out = append(out, PromSeries{Labels: labels, Value: v})
	}
	return out
}
