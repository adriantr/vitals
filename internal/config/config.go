// Package config loads the YAML that names an organisation's projects, repositories and the rules
// that classify their signals. The binary itself knows nothing about any one organisation.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/adriantr/vitals/internal/topology"
)

// ProjectID is a Google Cloud project id such as "acme-prod".
type ProjectID string

// Repo is a GitHub repository as owner/name.
type Repo string

type Config struct {
	DefaultEnv   string                 `yaml:"defaultEnv"`
	Environments map[string]Environment `yaml:"environments"`
	Thresholds   Thresholds             `yaml:"thresholds"`
	// The health model: signals (queries with thresholds), the services they attach to, and the
	// systems those services form. See model.go.
	Signals  map[string]Signal            `yaml:"signals"`
	Defaults map[ServiceKind]KindDefaults `yaml:"defaults"`
	Services map[string]Service           `yaml:"services"`
	Systems  map[string]System            `yaml:"systems"`
	// Optional: reads the repository to suggest and check service declarations.
	Topology *topology.Config `yaml:"topology"`
	// OTel metrics shown per component on top of the built-in OpenTelemetry semantic-convention set.
	Metrics []MetricSpec `yaml:"metrics"`
	// Directory of the config file; topology.root is resolved against it.
	Dir string `yaml:"-"`

	claims map[ClaimKind]map[string]string
}

type Environment struct {
	GCP    GCP     `yaml:"gcp"`
	GitHub *GitHub `yaml:"github"`
}

type GCP struct {
	// The project that runs the environment's services, SLOs, Pub/Sub and Error Reporting.
	Project ProjectID `yaml:"project"`
	// Every project whose alert policies count towards the environment's health.
	AlertProjects []ProjectID `yaml:"alertProjects"`
	// Cloud Run region. Defaults to europe-west1.
	Region string `yaml:"region"`
	// Error Reporting locations to read: "global" and the regions of any regional log buckets.
	// Defaults to global.
	ErrorLocations []ErrorLocation `yaml:"errorLocations"`
}

// ErrorLocation is an Error Reporting location: "global" or a region such as europe-west1.
type ErrorLocation string

type GitHub struct {
	Repo      Repo     `yaml:"repo"`
	Branch    string   `yaml:"branch"`
	Workflows []string `yaml:"workflows"`
}

type Thresholds struct {
	FlappingFires   int           `yaml:"flappingFires"`
	SLOSlowBurn     float64       `yaml:"sloSlowBurn"`
	SLOSevereBurn   float64       `yaml:"sloSevereBurn"`
	PubSubLagAmber  time.Duration `yaml:"pubsubLagAmber"`
	PubSubLagRed    time.Duration `yaml:"pubsubLagRed"`
	DeadLetterMatch string        `yaml:"deadLetterMatch"`
	// An open alert is chronic once its oldest incident opened this long before the window end.
	ChronicAlertAge time.Duration `yaml:"chronicAlertAge"`
	// A breached signal is chronic when it also breaches at the window end on each of this many
	// days before; 0 turns the check off.
	ChronicSignalDays int `yaml:"chronicSignalDays"`
}

// MetricSpec is one Cloud Monitoring metric read for a component, filtered to its OTel service names.
type MetricSpec struct {
	Label string `yaml:"label"`
	// Full metric type, e.g. workload.googleapis.com/http.server.request.duration.
	Type string `yaml:"type"`
	// sum (counters), p95 or p50 (distributions), max or mean (gauges).
	Stat string `yaml:"stat"`
	// Metric label to break the value down by; empty for one total.
	By string `yaml:"by"`
	// How many breakdown values to show, highest first. Defaults to 5.
	Top int `yaml:"top"`
	// ms, s, bytes or count; how the value is printed.
	Unit string `yaml:"unit"`
}

func defaults() Thresholds {
	return Thresholds{
		FlappingFires:     3,
		SLOSlowBurn:       2,
		SLOSevereBurn:     10,
		PubSubLagAmber:    30 * time.Minute,
		PubSubLagRed:      time.Hour,
		DeadLetterMatch:   ".*(dlq|dead).*",
		ChronicAlertAge:   24 * time.Hour,
		ChronicSignalDays: 2,
	}
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := Config{Thresholds: defaults()}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // a misspelt key would otherwise silently fall back to a default
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.Dir = filepath.Dir(path)
	if len(cfg.Environments) == 0 {
		return nil, fmt.Errorf("%s defines no environments", path)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := regexp.Compile(cfg.Thresholds.DeadLetterMatch); err != nil {
		return nil, fmt.Errorf("deadLetterMatch: %w", err)
	}
	return &cfg, nil
}

var ErrUnknownEnv = errors.New("unknown environment")

// Env resolves an environment by name; an empty name selects defaultEnv.
func (c *Config) Env(name string) (string, Environment, error) {
	if name == "" {
		name = c.DefaultEnv
	}
	env, ok := c.Environments[name]
	if !ok {
		known := make([]string, 0, len(c.Environments))
		for key := range c.Environments {
			known = append(known, key)
		}
		slices.Sort(known)
		return "", Environment{}, fmt.Errorf("%w %q; the config defines %s", ErrUnknownEnv, name, strings.Join(known, ", "))
	}
	if env.GCP.Project == "" {
		return "", Environment{}, fmt.Errorf("environment %q has no gcp.project", name)
	}
	if env.GCP.Region == "" {
		env.GCP.Region = "europe-west1"
	}
	if len(env.GCP.ErrorLocations) == 0 {
		env.GCP.ErrorLocations = []ErrorLocation{"global"}
	}
	if len(env.GCP.AlertProjects) == 0 {
		env.GCP.AlertProjects = []ProjectID{env.GCP.Project}
	}
	if env.GitHub != nil && env.GitHub.Branch == "" {
		env.GitHub.Branch = "main"
	}
	return name, env, nil
}
