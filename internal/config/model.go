package config

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Signal is one health measure: a PromQL query evaluated at the end of the window, with thresholds.
// The query may use {service}, {window}, {env} and {project}, plus any parameter the attachment sets.
type Signal struct {
	Description string   `yaml:"description"`
	PromQL      string   `yaml:"promql"`
	Amber       *float64 `yaml:"amber"`
	Red         *float64 `yaml:"red"`
	// Below makes falling under a threshold the breach, for rates that should not drop.
	Below bool `yaml:"below"`
	// ms, s, percent, bytes or count; how the value is printed.
	Unit string `yaml:"unit"`
	// What an empty result means: unknown (the default) or ok, for queries such as error counts
	// that return nothing when nothing failed.
	NoData string `yaml:"noData"`
	// By runs the signal once, grouped by this label; each service takes the series matching its name
	// or OTel names. The query must not use {service}.
	By string `yaml:"by"`
	// Logs names the log lines behind the signal. They are sampled only when the signal breaches.
	Logs *SignalLogs `yaml:"logs"`
}

// SignalLogs is a Cloud Logging filter whose newest lines are attached to a breached signal.
type SignalLogs struct {
	// A Logging query; {service}, {env}, {project} and attachment parameters are filled in. The
	// window's time range is added by vitals.
	Filter string `yaml:"filter"`
	// How many lines to attach to a finding, 1 to 20. Defaults to 3.
	Sample *int `yaml:"sample"`
}

const (
	DefaultLogSample = 3
	MaxLogSample     = 20
)

// SampleSize is the number of lines to attach, with the default applied.
func (l SignalLogs) SampleSize() int {
	if l.Sample == nil {
		return DefaultLogSample
	}
	return *l.Sample
}

// SignalRef attaches a signal. In YAML it is either the signal's name or a map with `use` and the
// template parameters, e.g. { use: topic-publishing, topic: orders }.
type SignalRef struct {
	Use    string
	Params map[string]string
}

func (r *SignalRef) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		r.Use = node.Value
		return nil
	}
	var m map[string]string
	if err := node.Decode(&m); err != nil {
		return fmt.Errorf("a signal reference is a name or a map of use and parameters: %w", err)
	}
	r.Use = m["use"]
	delete(m, "use")
	r.Params = m
	if r.Use == "" {
		return errors.New("a signal reference map needs `use`")
	}
	return nil
}

// Claims list, by exact name, the existing alerts and telemetry a service or system owns.
type Claims struct {
	// Alert policy display names.
	Alerts []string `yaml:"alerts"`
	// Cloud Monitoring service ids that hold SLOs.
	SLOServices []string `yaml:"sloServices"`
	// Error Reporting service names, matched without regard to case.
	Errors []string `yaml:"errors"`
	// Pub/Sub subscription ids.
	Subscriptions []string `yaml:"subscriptions"`
}

// KindDefaults holds what every service of one kind gets, e.g. the signals all Cloud Run services share.
type KindDefaults struct {
	Signals []SignalRef `yaml:"signals"`
}

// ServiceKind says how a service runs. cloud-run services are looked up in Cloud Run.
type ServiceKind string

const KindCloudRun ServiceKind = "cloud-run"

type Service struct {
	Description string `yaml:"description"`
	// cloud-run (the default) gets the deployed revision and Cloud Run request metrics; anything
	// else is declared but not looked up in Cloud Run.
	Kind ServiceKind `yaml:"kind"`
	// OTel service.name values its telemetry reports under.
	OTel      []string    `yaml:"otel"`
	Signals   []SignalRef `yaml:"signals"`
	DependsOn []string    `yaml:"dependsOn"`
	Claims    `yaml:",inline"`
}

type System struct {
	Description string      `yaml:"description"`
	Services    []string    `yaml:"services"`
	Signals     []SignalRef `yaml:"signals"`
	Claims      `yaml:",inline"`
}

// ServiceKind is the service's kind with the default applied.
func (s Service) ServiceKind() ServiceKind {
	if s.Kind == "" {
		return KindCloudRun
	}
	return s.Kind
}

// AllSignals is the kind's default signals followed by the service's own; a service attaching a
// default signal itself (e.g. with its own thresholds) replaces the default attachment.
func (c *Config) AllSignals(name string) []SignalRef {
	svc := c.Services[name]
	var out []SignalRef
	for _, def := range c.Defaults[svc.ServiceKind()].Signals {
		if !slices.ContainsFunc(svc.Signals, func(own SignalRef) bool { return own.Use == def.Use }) {
			out = append(out, def)
		}
	}
	return append(out, svc.Signals...)
}

// IsService reports whether an owner name is a declared service (rather than a system).
func (c *Config) IsService(name string) bool {
	_, ok := c.Services[name]
	return ok
}

// SystemsOf lists the systems a service belongs to.
func (c *Config) SystemsOf(service string) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(c.Systems)) {
		if slices.Contains(c.Systems[name].Services, service) {
			out = append(out, name)
		}
	}
	return out
}

type ClaimKind string

const (
	ClaimAlert        ClaimKind = "alert"
	ClaimSLOService   ClaimKind = "sloService"
	ClaimError        ClaimKind = "error"
	ClaimSubscription ClaimKind = "subscription"
)

// Owner returns the service or system that claims a name, or "" when nobody does.
func (c *Config) Owner(kind ClaimKind, name string) string {
	if kind == ClaimError {
		name = strings.ToLower(name)
	}
	return c.claims[kind][name]
}

// Claimed lists every claimed name of one kind with its owner.
func (c *Config) Claimed(kind ClaimKind) map[string]string { return c.claims[kind] }

// ThresholdParam reports whether an attachment parameter overrides a threshold rather than filling
// the query.
func ThresholdParam(key string) bool { return key == "amber" || key == "red" }

// Override applies an attachment's amber/red parameters to the signal's thresholds.
func (sig *Signal) Override(params map[string]string) error {
	for key, target := range map[string]**float64{"amber": &sig.Amber, "red": &sig.Red} {
		text, ok := params[key]
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return fmt.Errorf("%s threshold %q is not a number", key, text)
		}
		*target = &v
	}
	return nil
}

var placeholder = regexp.MustCompile(`\{(\w+)\}`)

var builtinParams = map[string]bool{"service": true, "window": true, "env": true, "project": true}

var analyticsTable = regexp.MustCompile(`^[\w.:-]+\.\w+\.\w+$`)

// A log filter gets its time range from vitals, so {window} has no meaning there.
var logParams = map[string]bool{"service": true, "env": true, "project": true}

func (l SignalLogs) validate(signal string) []string {
	var problems []string
	if strings.TrimSpace(l.Filter) == "" {
		problems = append(problems, fmt.Sprintf("signal %s: logs.filter is required when logs is set", signal))
	}
	if strings.Contains(l.Filter, "{window}") {
		problems = append(problems, fmt.Sprintf("signal %s: logs.filter must not use {window}; vitals adds the window's time range", signal))
	}
	if l.Sample != nil && (*l.Sample < 1 || *l.Sample > MaxLogSample) {
		problems = append(problems, fmt.Sprintf("signal %s: logs.sample must be between 1 and %d", signal, MaxLogSample))
	}
	return problems
}

// Expand fills a signal's query; it fails when a placeholder has no value.
func (sig Signal) Expand(values map[string]string) (string, error) { return Fill(sig.PromQL, values) }

// ExpandLogs fills a signal's log filter; "" when the signal has no logs block.
func (sig Signal) ExpandLogs(values map[string]string) (string, error) {
	if sig.Logs == nil {
		return "", nil
	}
	return Fill(sig.Logs.Filter, values)
}

// Fill replaces every {name} in text; it fails when a placeholder has no value.
func Fill(text string, values map[string]string) (string, error) {
	var missing []string
	out := placeholder.ReplaceAllStringFunc(text, func(m string) string {
		key := m[1 : len(m)-1]
		v, ok := values[key]
		if !ok {
			missing = append(missing, key)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("no value for {%s}", strings.Join(missing, "}, {"))
	}
	return out, nil
}

func (c *Config) validate() error {
	var problems []string
	if c.Thresholds.FlappingFires < 1 {
		problems = append(problems, "thresholds.flappingFires must be at least 1")
	}
	if c.Thresholds.ChronicAlertAge <= 0 {
		problems = append(problems, "thresholds.chronicAlertAge must be positive")
	}
	if c.Thresholds.ChronicSignalDays < 0 {
		problems = append(problems, "thresholds.chronicSignalDays must not be negative")
	}
	for name, sig := range c.Signals {
		if sig.PromQL == "" {
			problems = append(problems, fmt.Sprintf("signal %s has no promql", name))
		}
		if sig.By != "" && strings.Contains(sig.PromQL, "{service}") {
			problems = append(problems, fmt.Sprintf("signal %s groups by %s, so it must not use {service}", name, sig.By))
		}
		if sig.NoData != "" && sig.NoData != "unknown" && sig.NoData != "ok" {
			problems = append(problems, fmt.Sprintf("signal %s: noData must be unknown or ok", name))
		}
		if sig.Logs != nil {
			problems = append(problems, sig.Logs.validate(name)...)
		}
	}
	checkRefs := func(owner string, refs []SignalRef, isService bool) {
		for _, ref := range refs {
			sig, ok := c.Signals[ref.Use]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s uses undefined signal %q", owner, ref.Use))
				continue
			}
			given := func(key string) bool {
				_, ok := ref.Params[key]
				return ok && !ThresholdParam(key)
			}
			for _, m := range placeholder.FindAllStringSubmatch(sig.PromQL, -1) {
				key := m[1]
				if given(key) || (builtinParams[key] && (key != "service" || isService)) {
					continue
				}
				problems = append(problems, fmt.Sprintf("%s uses signal %s without a value for {%s}", owner, ref.Use, key))
			}
			if sig.Logs == nil {
				continue
			}
			for _, m := range placeholder.FindAllStringSubmatch(sig.Logs.Filter, -1) {
				key := m[1]
				if given(key) || (logParams[key] && (key != "service" || isService)) {
					continue
				}
				problems = append(problems, fmt.Sprintf("%s uses signal %s whose logs.filter has no value for {%s}", owner, ref.Use, key))
			}
		}
	}
	for kind, k := range c.Defaults {
		checkRefs("defaults."+string(kind), k.Signals, true)
	}
	for name, svc := range c.Services {
		checkRefs("service "+name, svc.Signals, true)
		for _, dep := range svc.DependsOn {
			if _, ok := c.Services[dep]; !ok {
				if _, ok := c.Systems[dep]; !ok {
					problems = append(problems, fmt.Sprintf("service %s depends on undeclared %q", name, dep))
				}
			}
		}
	}
	for name, sys := range c.Systems {
		if _, clash := c.Services[name]; clash {
			problems = append(problems, fmt.Sprintf("%q is both a service and a system", name))
		}
		checkRefs("system "+name, sys.Signals, false)
		for _, svc := range sys.Services {
			if _, ok := c.Services[svc]; !ok {
				problems = append(problems, fmt.Sprintf("system %s lists undeclared service %q", name, svc))
			}
		}
	}
	if view := c.Logs.AnalyticsView; view != "" {
		filled, err := Fill(view, map[string]string{"env": "e", "project": "p"})
		if err != nil {
			problems = append(problems, fmt.Sprintf("logs.analyticsView may only use {project} and {env}: %v", err))
		} else if !analyticsTable.MatchString(filled) {
			problems = append(problems, fmt.Sprintf("logs.analyticsView %q is not project.dataset.view", view))
		}
	}
	if err := c.indexClaims(); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func (c *Config) indexClaims() error {
	c.claims = map[ClaimKind]map[string]string{}
	var clashes []string
	add := func(owner string, claims Claims) {
		for kind, names := range map[ClaimKind][]string{ClaimAlert: claims.Alerts, ClaimSLOService: claims.SLOServices, ClaimError: claims.Errors, ClaimSubscription: claims.Subscriptions} {
			if c.claims[kind] == nil {
				c.claims[kind] = map[string]string{}
			}
			for _, name := range names {
				if kind == ClaimError {
					name = strings.ToLower(name)
				}
				if prev, ok := c.claims[kind][name]; ok && prev != owner {
					clashes = append(clashes, fmt.Sprintf("%s %q is claimed by both %s and %s", kind, name, prev, owner))
				}
				c.claims[kind][name] = owner
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Services)) {
		add(name, c.Services[name].Claims)
	}
	for _, name := range slices.Sorted(maps.Keys(c.Systems)) {
		add(name, c.Systems[name].Claims)
	}
	if len(clashes) > 0 {
		return errors.New(strings.Join(clashes, "; "))
	}
	return nil
}
