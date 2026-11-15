// Package report holds what sources conclude and turns it into a snapshot.
package report

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/query"
)

// Status is one health level for findings, signals, services, systems and the whole snapshot.
type Status string

const (
	OK      Status = "ok"
	Unknown Status = "unknown"
	Amber   Status = "amber"
	Red     Status = "red"
)

var severity = map[Status]int{OK: 0, Unknown: 1, Amber: 2, Red: 3}

// Worse returns the more severe of two statuses.
func (s Status) Worse(other Status) Status {
	if severity[other] > severity[s] {
		return other
	}
	return s
}

// Breach reports whether the status calls for attention.
func (s Status) Breach() bool { return s == Red || s == Amber }

// WorstFirst orders statuses for sorting, most severe first.
func WorstFirst(a, b Status) int { return severity[b] - severity[a] }

// RefKind names what a Ref points at; `vitals explain` drills into each kind.
type RefKind string

const (
	RefPolicy       RefKind = "policy"
	RefSLO          RefKind = "slo"
	RefError        RefKind = "error"
	RefSubscription RefKind = "pubsub"
	RefWorkflow     RefKind = "workflow"
	RefSignal       RefKind = "signal"
	RefService      RefKind = "service"
)

// Ref names a finding's subject as kind:target, e.g. policy:Checkout offline.
type Ref string

func NewRef(kind RefKind, target string) Ref { return Ref(string(kind) + ":" + target) }

// Parts splits a ref into its kind and target.
func (r Ref) Parts() (RefKind, string) {
	kind, target, _ := strings.Cut(string(r), ":")
	return RefKind(kind), target
}

type Finding struct {
	Level  Status `json:"level"`
	Source string `json:"source"`
	// The Cloud Run service the source itself names (an alert's service label, the SLO's service),
	// before ownership is resolved.
	Service string `json:"serviceHint,omitempty"`
	// Who the finding belongs to: a mapped service or a system from the config's owners. Empty when
	// neither the signal nor the config says.
	Owner string `json:"owner,omitempty"`
	// OwnerIsService is true when Owner is a Cloud Run service from the system map.
	OwnerIsService bool   `json:"ownerIsService"`
	Title          string `json:"title"`
	Detail         string `json:"detail"`
	// When the condition started, if the source knows.
	Since *time.Time `json:"since,omitempty"`
	// Chronic findings have held for longer than the window (see thresholds.chronicAlertAge and
	// chronicSignalDays). They count in status like any other but render in their own section.
	Chronic bool `json:"chronic,omitempty"`
	// Pass to `vitals explain` to drill into this finding.
	Ref Ref `json:"ref"`
}

type ChangeKind string

const (
	Deploy  ChangeKind = "deploy"
	Release ChangeKind = "release"
)

type Change struct {
	At   time.Time  `json:"at"`
	Kind ChangeKind `json:"kind"`
	What string     `json:"what"`
	By   string     `json:"by,omitempty"`
}

// Report is one source's answer. Checked tells "nothing wrong" apart from "looked at nothing".
type Report struct {
	Source      string        `json:"source"`
	Status      query.Status  `json:"status"`
	Problems    []string      `json:"problems"`
	Checked     int           `json:"checked"`
	CheckedWhat string        `json:"checkedWhat"`
	Findings    []Finding     `json:"-"`
	Changes     []Change      `json:"-"`
	Values      []SignalValue `json:"-"`
	Notes       []string      `json:"notes"`
}

// SignalValue is one declared signal evaluated for one owner (a service or a system).
type SignalValue struct {
	Owner  string `json:"owner"`
	Signal string `json:"signal"`
	// The attachment's parameters, e.g. topic=reviews-prod-high, when it has any.
	Params string   `json:"params,omitempty"`
	Value  *float64 `json:"value"`
	// Labels of the worst series when the query returns several (per job, host, topic, …).
	Series      string `json:"series,omitempty"`
	SeriesCount int    `json:"seriesCount,omitempty"`
	// Unknown when the query returned nothing (unless the signal says that is ok) or failed.
	Level Status `json:"level"`
	Unit  string `json:"unit,omitempty"`
	Query string `json:"query"`
}

type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func (w Window) Duration() time.Duration { return w.End.Sub(w.Start) }

func (w Window) Hours() float64 { return w.Duration().Hours() }

func (w Window) Minutes() float64 { return w.Duration().Minutes() }

type Snapshot struct {
	Env         string    `json:"env"`
	GeneratedAt time.Time `json:"generatedAt"`
	Window      Window    `json:"window"`
	Overall     Status    `json:"overall"`
	// Sources that did not answer: their signals are unknown, not green.
	Blind    []string  `json:"blind"`
	Sources  []Report  `json:"sources"`
	Findings []Finding `json:"findings"`
	Changes  []Change  `json:"changes"`
	// SchemaVersion changes when the JSON shape does.
	SchemaVersion int `json:"schemaVersion"`
	// Historical is true when the window ends in the past; deployed state is then left out.
	Historical bool `json:"historical"`
	// One row per declared service, worst status first.
	Services []ServiceRow `json:"services"`
	// One row per declared system, worst status first.
	Systems []SystemRow `json:"systems,omitempty"`
	// Every declared signal's value for its owner.
	Signals []SignalValue `json:"signals"`
}

// ServiceRow is one declared service at a glance. Nil metrics mean no Cloud Run request data.
type ServiceRow struct {
	Name       string             `json:"name"`
	Kind       config.ServiceKind `json:"kind"`
	Systems    []string           `json:"systems"`
	Status     Status             `json:"status"`
	DeployedAt *time.Time         `json:"deployedAt,omitempty"`
	Commit     string             `json:"commit,omitempty"`
	Ready      bool               `json:"ready"`
	Missing    bool               `json:"missingFromCloudRun"`
	PerMinute  *float64           `json:"requestsPerMinute,omitempty"`
	ErrorRatio *float64           `json:"serverErrorRatio,omitempty"`
	P95ms      *float64           `json:"p95Ms,omitempty"`
	Instances  *float64           `json:"peakInstances,omitempty"`
	Red        int                `json:"red"`
	Amber      int                `json:"amber"`
	// Of Red and Amber, how many are chronic.
	Chronic        int `json:"chronic"`
	UnknownSignals int `json:"unknownSignals"`
}

// SystemRow is one declared system: its rolled-up status and which services pull it down.
type SystemRow struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Status      Status   `json:"status"`
	Services    int      `json:"services"`
	Degraded    []string `json:"degraded"`
	// Findings owned by the system itself rather than one of its services.
	Own int `json:"ownFindings"`
}

// Tracker makes one source's reads and remembers why any of them failed. Safe for concurrent use.
type Tracker struct {
	mu       sync.Mutex
	source   string
	client   *query.Client
	status   query.Status
	problems []string
}

func NewTracker(source string, client *query.Client) *Tracker {
	return &Tracker{source: source, client: client, status: query.OK}
}

func (t *Tracker) JSON(ctx context.Context, title string, u *url.URL, body any, cred query.Credential, out any) bool {
	err := t.client.JSON(ctx, t.source+": "+title, u, body, cred, out)
	if err == nil {
		return true
	}
	var qe *query.Error
	if errors.As(err, &qe) {
		t.Fail(qe.Status, fmt.Sprintf("%s: %s: %s", title, qe.Exit, FirstLine(qe.Message, 200)))
	} else {
		t.Fail(query.Failed, fmt.Sprintf("%s: %s", title, err))
	}
	return false
}

// Status is the worst outcome of the reads so far.
func (t *Tracker) Status() query.Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (t *Tracker) Fail(status query.Status, problem string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.problems = append(t.problems, problem)
	if t.status == query.OK || status == query.Blocked {
		t.status = status
	}
}

// Problems lists why reads failed so far.
func (t *Tracker) Problems() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.problems)
}

// WithValues attaches evaluated signals to a report.
func (r Report) WithValues(values []SignalValue) Report {
	r.Values = values
	return r
}

func (t *Tracker) Report(checked int, checkedWhat string, findings []Finding, changes []Change, notes []string) Report {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Report{
		Source: t.source, Status: t.status, Problems: nonNil(t.problems),
		Checked: checked, CheckedWhat: checkedWhat,
		Findings: findings, Changes: changes, Notes: nonNil(notes),
	}
}

func Assemble(env string, window Window, reports []Report, now time.Time) Snapshot {
	snap := Snapshot{
		Env: env, GeneratedAt: now.UTC(), Window: window, Overall: OK,
		Blind: []string{}, Sources: reports, Findings: []Finding{}, Changes: []Change{},
	}
	for _, r := range reports {
		snap.Findings = append(snap.Findings, r.Findings...)
		snap.Changes = append(snap.Changes, r.Changes...)
		if r.Status != query.OK {
			snap.Blind = append(snap.Blind, r.Source)
		}
	}
	slices.SortStableFunc(snap.Findings, func(a, b Finding) int {
		if a.Level != b.Level {
			return WorstFirst(a.Level, b.Level)
		}
		return compareSinceDesc(a.Since, b.Since)
	})
	SortChanges(snap.Changes)
	for _, f := range snap.Findings {
		snap.Overall = snap.Overall.Worse(f.Level)
	}
	// No findings with a blind source is not health.
	if snap.Overall == OK && len(snap.Blind) > 0 {
		snap.Overall = Unknown
	}
	return snap
}

func SortChanges(changes []Change) {
	slices.SortStableFunc(changes, func(a, b Change) int { return b.At.Compare(a.At) })
}

func compareSinceDesc(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	default:
		return b.Compare(*a)
	}
}

func FirstLine(text string, maxRunes int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	runes := []rune(line)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes-1]) + "…"
	}
	return line
}

func nonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}
