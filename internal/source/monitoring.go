package source

import (
	"context"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
)

// Reading is one point of a time series.
type Reading struct {
	At    time.Time
	Value float64
}

type timeSeries struct {
	Metric struct {
		Labels map[string]string `json:"labels"`
	} `json:"metric"`
	Resource struct {
		Labels map[string]string `json:"labels"`
	} `json:"resource"`
	Points []struct {
		Interval struct {
			EndTime string `json:"endTime"`
		} `json:"interval"`
		Value struct {
			DoubleValue *float64 `json:"doubleValue"`
			Int64Value  *string  `json:"int64Value"`
		} `json:"value"`
	} `json:"points"`
}

type seriesList struct {
	TimeSeries []timeSeries `json:"timeSeries"`
}

// labels merges a series' resource and metric labels.
func (ts timeSeries) labels() map[string]string {
	out := maps.Clone(ts.Resource.Labels)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, ts.Metric.Labels)
	return out
}

// readings returns the series' points, oldest first.
func (ts timeSeries) readings() []Reading {
	var out []Reading
	for _, p := range ts.Points {
		value := math.NaN()
		if p.Value.DoubleValue != nil {
			value = *p.Value.DoubleValue
		} else if p.Value.Int64Value != nil {
			value, _ = strconv.ParseFloat(*p.Value.Int64Value, 64)
		}
		at := parseTime(p.Interval.EndTime)
		if at == nil || math.IsNaN(value) {
			continue
		}
		out = append(out, Reading{At: *at, Value: value})
	}
	slices.SortFunc(out, func(a, b Reading) int { return a.At.Compare(b.At) })
	return out
}

// readings flattens every series' points, oldest first.
func (l seriesList) readings() []Reading {
	var out []Reading
	for _, ts := range l.TimeSeries {
		out = append(out, ts.readings()...)
	}
	slices.SortFunc(out, func(a, b Reading) int { return a.At.Compare(b.At) })
	return out
}

func SeriesURL(project, filter string, w report.Window, extra url.Values) *url.URL {
	params := url.Values{"filter": {filter}, "interval.startTime": {iso(w.Start)}, "interval.endTime": {iso(w.End)}}
	maps.Copy(params, extra)
	return endpoint(fmt.Sprintf("https://monitoring.googleapis.com/v3/projects/%s/timeSeries", project), params)
}

// querySeries reads one Cloud Monitoring time-series filter over w in the environment's project;
// extra carries aggregation parameters, nil reads raw points.
func querySeries(ctx context.Context, s Scope, t *report.Tracker, title, filter string, w report.Window, extra url.Values) (seriesList, bool) {
	ok := true
	series := paged(t, title, func(token string) ([]timeSeries, string, bool) {
		var page struct {
			TimeSeries    []timeSeries `json:"timeSeries"`
			NextPageToken string       `json:"nextPageToken"`
		}
		params := maps.Clone(extra)
		if params == nil {
			params = url.Values{}
		}
		if token != "" {
			params.Set("pageToken", token)
		}
		ok = t.JSON(ctx, title, SeriesURL(string(s.Environment.GCP.Project), filter, w, params), nil, s.Google, &page)
		return page.TimeSeries, page.NextPageToken, ok
	})
	return seriesList{TimeSeries: series}, ok
}

// Series reads one time-series filter over w, oldest point first.
func Series(ctx context.Context, s Scope, t *report.Tracker, title, filter string, w report.Window, extra url.Values) ([]Reading, bool) {
	list, ok := querySeries(ctx, s, t, title, filter, w, extra)
	return list.readings(), ok
}

// Aggregation reduces one metric over the whole window to a single value per group.
type Aggregation struct {
	Aligner string // per series, e.g. ALIGN_DELTA, ALIGN_MAX
	Reducer string // across series, e.g. REDUCE_SUM, REDUCE_PERCENTILE_95
	GroupBy []string
}

// Grouped is one group's labels (resource and metric labels merged) and its value.
type Grouped struct {
	Labels map[string]string
	Value  float64
}

// GroupedQuery reads one value per group for the window, using a single alignment period that spans it.
func GroupedQuery(ctx context.Context, s Scope, t *report.Tracker, title, filter string, w report.Window, agg Aggregation) ([]Grouped, bool) {
	extra := url.Values{
		"aggregation.alignmentPeriod":    {fmt.Sprintf("%ds", int(w.Duration().Seconds()))},
		"aggregation.perSeriesAligner":   {agg.Aligner},
		"aggregation.crossSeriesReducer": {agg.Reducer},
		"aggregation.groupByFields":      agg.GroupBy,
	}
	list, ok := querySeries(ctx, s, t, title, filter, w, extra)
	if !ok {
		return nil, false
	}
	var out []Grouped
	for _, ts := range list.TimeSeries {
		if points := ts.readings(); len(points) > 0 {
			out = append(out, Grouped{Labels: ts.labels(), Value: points[len(points)-1].Value})
		}
	}
	return out, true
}

// OneOf renders a Monitoring filter clause matching any of values for a label path.
func OneOf(path string, values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	if len(quoted) == 1 {
		return fmt.Sprintf("%s = %s", path, quoted[0])
	}
	return fmt.Sprintf("%s = one_of(%s)", path, strings.Join(quoted, ", "))
}

// Shifted moves a window back by the given number of days, for a same-time-of-day baseline.
func Shifted(w report.Window, days int) report.Window {
	d := w.End.Sub(w.End.AddDate(0, 0, -days))
	return report.Window{Start: w.Start.Add(-d), End: w.End.Add(-d)}
}

// paged collects a paginated listing. fetch returns one page's items and the next page token; a
// listing still unfinished after maxPages is recorded as a blind spot rather than silently cut.
func paged[T any](t *report.Tracker, title string, fetch func(token string) ([]T, string, bool)) []T {
	var all []T
	token := ""
	for range maxPages {
		items, next, ok := fetch(token)
		if !ok {
			return all
		}
		all = append(all, items...)
		if token = next; token == "" {
			return all
		}
	}
	t.Fail(query.Failed, fmt.Sprintf("%s: stopped after %d pages; the rest is not shown", title, maxPages))
	return all
}

// pageParams adds pageSize and, after the first page, pageToken to a listing's parameters.
func pageParams(token string, params url.Values) url.Values {
	if params == nil {
		params = url.Values{}
	}
	params.Set("pageSize", "500")
	if token != "" {
		params.Set("pageToken", token)
	}
	return params
}
