// Package source reads each system that carries a health signal and reports red and amber findings.
package source

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
)

// Cache holds reads that several sources make in one run, so each is made once.
type Cache struct {
	runServices once[map[string]RunService]
}

// once runs a read a single time with its own tracker; every caller gets the value and has any
// failure of the read reported on its own tracker too.
type once[T any] struct {
	do       sync.Once
	value    T
	status   query.Status
	problems []string
}

func (o *once[T]) get(s Scope, t *report.Tracker, title string, read func(*report.Tracker) T) T {
	o.do.Do(func() {
		inner := report.NewTracker(title, s.Client)
		o.value = read(inner)
		r := inner.Report(0, "", nil, nil, nil)
		o.status, o.problems = r.Status, r.Problems
	})
	for _, p := range o.problems {
		t.Fail(o.status, p)
	}
	return o.value
}

type Scope struct {
	Cache       *Cache
	Env         string
	Environment config.Environment
	Config      *config.Config
	Window      report.Window
	Client      *query.Client
	Google      query.Credential
	GitHub      query.Credential
}

type Collector func(ctx context.Context, s Scope) report.Report

func endpoint(base string, params url.Values) *url.URL {
	u, err := url.Parse(base)
	if err != nil {
		panic(err) // every base is a constant in this package
	}
	u.RawQuery = params.Encode()
	return u
}

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// at parses an RFC 3339 timestamp for comparison; an unparsable one is the zero time.
func at(text string) time.Time {
	if t := parseTime(text); t != nil {
		return *t
	}
	return time.Time{}
}

func parseTime(text string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return nil
	}
	return &t
}

// parallel runs fn over items concurrently and returns the results in input order.
func parallel[T, R any](items []T, fn func(T) R) []R {
	results := make([]R, len(items))
	tasks := make([]func(), len(items))
	for i, item := range items {
		tasks[i] = func() { results[i] = fn(item) }
	}
	Concurrently(tasks...)
	return results
}

// Concurrently runs every task at once and returns when all have finished.
func Concurrently(tasks ...func()) {
	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Go(task)
	}
	wg.Wait()
}
