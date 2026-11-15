// Package topology maps what the repository declares (Terraform locals, Knative manifests, release
// config, catalogues); every fact keeps the file it came from.
package topology

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/adriantr/vitals/internal/hclx"
)

// Config names the anchors. Paths are relative to Root, itself relative to the config file.
type Config struct {
	Root     string            `yaml:"root" json:"-"`
	Vars     map[string]string `yaml:"vars" json:"-"`
	Services Local             `yaml:"services" json:"-"`
	// Cloud Run service names that differ from their deploy pipeline's key.
	ServiceNames map[string]string `yaml:"serviceNames" json:"-"`
	Manifests    []string          `yaml:"manifests" json:"-"`
	// Container env vars worth showing on component pages, keyed by variable name, valued by label.
	ManifestEnv map[string]string `yaml:"manifestEnv" json:"-"`
	Access      *Local            `yaml:"access" json:"-"`
	Hosts       *HostsConfig      `yaml:"hosts" json:"-"`
	Push        *Local            `yaml:"push" json:"-"`
	Agents      string            `yaml:"agents" json:"-"`
	Releases    *Releases         `yaml:"releases" json:"-"`
	Jobs        *Jobs             `yaml:"jobs" json:"-"`
}

// Local is one `locals` entry of a Terraform root module.
type Local struct {
	Dir   string `yaml:"dir"`
	Local string `yaml:"local"`
}

type HostsConfig struct {
	Local  `yaml:",inline"`
	Select []string `yaml:"select"`
}

type Releases struct {
	Manifest   string            `yaml:"manifest"`
	PackageFor map[string]string `yaml:"packageFor"`
}

type Jobs struct {
	Catalogue string `yaml:"catalogue"`
	// Regexes whose first group names a job that code launches directly.
	Launched []JobPattern `yaml:"launched"`
}

type JobPattern struct {
	Glob    string `yaml:"glob"`
	Pattern string `yaml:"pattern"`
}

type Map struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Env         string    `json:"env"`
	Services    []Service `json:"services"`
	Agents      []Agent   `json:"agents"`
	Packages    []Package `json:"packages"`
	BatchJobs   []string  `json:"batchJobs"`
	Launched    []Job     `json:"launchedJobs"`
	// Anchors that could not be read or facts that did not resolve, so gaps are visible.
	Warnings []string `json:"warnings"`
}

type Service struct {
	Name           string      `json:"name"`
	Repo           string      `json:"repo"`
	Package        string      `json:"package,omitempty"`
	Manifest       string      `json:"manifest,omitempty"`
	Ingress        string      `json:"ingress,omitempty"`
	ServiceAccount string      `json:"serviceAccount,omitempty"`
	Scaling        string      `json:"scaling,omitempty"`
	Containers     []Container `json:"containers"`
	Hosts          []string    `json:"hosts"`
	Spanner        []string    `json:"spanner"`
	Publishes      []string    `json:"publishes"`
	Pulls          []string    `json:"pulls"`
	Buckets        []string    `json:"buckets"`
	Invokes        []string    `json:"invokes"`
	InvokedBy      []string    `json:"invokedBy"`
	PushFrom       []Push      `json:"pushFrom"`
	SidecarIn      []string    `json:"sidecarIn"`
	Sources        []string    `json:"sources"`
}

// OTelNames lists the service.name values its containers report telemetry under.
func (s Service) OTelNames() []string {
	var names []string
	for _, c := range s.Containers {
		if c.OTelName != "" && !slices.Contains(names, c.OTelName) {
			names = append(names, c.OTelName)
		}
	}
	return names
}

type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// Serving is the container that receives requests (the one declaring a port); the rest are sidecars.
	Serving  bool      `json:"serving"`
	OTelName string    `json:"otelName,omitempty"`
	Env      []EnvFact `json:"env,omitempty"`
	Calls    []string  `json:"calls"`
	Secrets  []string  `json:"secrets"`
}

// EnvFact is a container env var selected by the topology's manifestEnv.
type EnvFact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type Push struct {
	Topic string `json:"topic"`
	Path  string `json:"path,omitempty"`
}

type Agent struct {
	Name     string   `json:"name"`
	Invokers []string `json:"invokers"`
	Durable  bool     `json:"durable"`
}

type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Job struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// Build reads every configured anchor under root.
func Build(cfg Config, root string, now time.Time) (*Map, error) {
	vars := map[string]any{}
	for k, v := range cfg.Vars {
		vars[k] = v
	}
	b := &builder{root: root, vars: vars, stacks: map[string]*hclx.Stack{}, env: cfg.ManifestEnv}
	m := &Map{GeneratedAt: now.UTC(), Env: cfg.Vars["env"]}
	services, err := b.services(cfg.Services, cfg.ServiceNames)
	if err != nil {
		return nil, err
	}
	b.manifests(cfg.Manifests, services)
	if cfg.Access != nil {
		b.access(*cfg.Access, services)
	}
	if cfg.Hosts != nil {
		b.hosts(*cfg.Hosts, services)
	}
	if cfg.Push != nil {
		b.push(*cfg.Push, services)
	}
	if cfg.Releases != nil {
		m.Packages = b.releases(*cfg.Releases, services)
	}
	if cfg.Agents != "" {
		m.Agents = b.agents(cfg.Agents)
	}
	if cfg.Jobs != nil {
		m.BatchJobs, m.Launched = b.jobs(*cfg.Jobs)
	}
	linkReverse(services)
	for _, name := range slices.Sorted(maps.Keys(services)) {
		m.Services = append(m.Services, *services[name])
	}
	for _, stack := range b.stacks {
		b.warnings = append(b.warnings, stack.Warnings...)
	}
	slices.Sort(b.warnings)
	m.Warnings = slices.Compact(b.warnings)
	return m, nil
}

type builder struct {
	root     string
	vars     map[string]any
	stacks   map[string]*hclx.Stack
	env      map[string]string
	warnings []string
}

func (b *builder) warn(format string, args ...any) {
	b.warnings = append(b.warnings, fmt.Sprintf(format, args...))
}

func (b *builder) rel(path string) string {
	if r, err := filepath.Rel(b.root, path); err == nil {
		return r
	}
	return path
}

func (b *builder) local(l Local) (any, string, error) {
	dir := filepath.Join(b.root, l.Dir)
	stack, ok := b.stacks[dir]
	if !ok {
		var err error
		if stack, err = hclx.Load(dir, b.vars); err != nil {
			return nil, "", err
		}
		b.stacks[dir] = stack
	}
	v, err := stack.Local(l.Local)
	if err != nil {
		return nil, "", err
	}
	return v, b.rel(stack.Location(l.Local)), nil
}

func (b *builder) services(l Local, rename map[string]string) (map[string]*Service, error) {
	v, at, err := b.local(l)
	if err != nil {
		return nil, fmt.Errorf("services: %w", err)
	}
	out := map[string]*Service{}
	for _, repo := range slices.Sorted(maps.Keys(hclx.Map(v))) {
		for _, name := range slices.Sorted(maps.Keys(hclx.Map(hclx.Map(v)[repo]))) {
			svc := cmp.Or(rename[name], name)
			out[svc] = &Service{Name: svc, Repo: repo, Sources: []string{at}}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("services: local.%s in %s resolved to no services", l.Local, l.Dir)
	}
	return out, nil
}

type knative struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Metadata struct {
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
			Spec struct {
				ServiceAccountName string `yaml:"serviceAccountName"`
				Containers         []struct {
					Name  string `yaml:"name"`
					Image string `yaml:"image"`
					Ports []any  `yaml:"ports"`
					Env   []struct {
						Name      string `yaml:"name"`
						Value     string `yaml:"value"`
						ValueFrom *struct {
							SecretKeyRef *struct {
								Name string `yaml:"name"`
							} `yaml:"secretKeyRef"`
						} `yaml:"valueFrom"`
					} `yaml:"env"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

var urlValue = regexp.MustCompile(`^https?://[^\s]+$`)

func (b *builder) manifests(globs []string, services map[string]*Service) {
	for _, glob := range globs {
		paths, err := filepath.Glob(filepath.Join(b.root, glob))
		if err != nil || len(paths) == 0 {
			b.warn("manifests: %s matched no files", glob)
			continue
		}
		for _, path := range paths {
			b.manifest(path, services)
		}
	}
}

func (b *builder) manifest(path string, services map[string]*Service) {
	raw, err := os.ReadFile(path)
	if err != nil {
		b.warn("manifest %s: %v", b.rel(path), err)
		return
	}
	var k knative
	if err := yaml.Unmarshal(raw, &k); err != nil || k.Kind != "Service" {
		return
	}
	svc, ok := services[k.Metadata.Name]
	if !ok {
		b.warn("manifest %s names service %q, which the services anchor does not list", b.rel(path), k.Metadata.Name)
		return
	}
	if svc.Manifest != "" {
		return
	}
	svc.Manifest = b.rel(path)
	svc.Ingress = k.Metadata.Annotations["run.googleapis.com/ingress"]
	ann := k.Spec.Template.Metadata.Annotations
	if lo, hi := ann["autoscaling.knative.dev/minScale"], ann["autoscaling.knative.dev/maxScale"]; lo != "" || hi != "" {
		svc.Scaling = fmt.Sprintf("%s–%s instances", cmp.Or(lo, "0"), cmp.Or(hi, "?"))
	}
	svc.ServiceAccount = k.Spec.Template.Spec.ServiceAccountName
	for _, c := range k.Spec.Template.Spec.Containers {
		ct := Container{Name: c.Name, Image: c.Image, Serving: len(c.Ports) > 0 || len(k.Spec.Template.Spec.Containers) == 1, Calls: []string{}, Secrets: []string{}}
		for _, e := range c.Env {
			switch {
			case e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil:
				ct.Secrets = append(ct.Secrets, e.Name)
			case e.Name == "OTEL_SERVICE_NAME":
				ct.OTelName = e.Value
			case b.env[e.Name] != "":
				ct.Env = append(ct.Env, EnvFact{Label: b.env[e.Name], Value: e.Value})
			case urlValue.MatchString(e.Value) && !strings.Contains(e.Value, "localhost") && !strings.HasPrefix(e.Name, "OTEL_"):
				ct.Calls = append(ct.Calls, e.Name+"="+e.Value)
			}
		}
		svc.Containers = append(svc.Containers, ct)
	}
	svc.Sources = append(svc.Sources, svc.Manifest)
}

func (b *builder) access(l Local, services map[string]*Service) {
	v, at, err := b.local(l)
	if err != nil {
		b.warn("access: %v", err)
		return
	}
	for name, raw := range hclx.Map(v) {
		svc, ok := services[name]
		if !ok {
			b.warn("access: local.%s lists %q, which the services anchor does not", l.Local, name)
			continue
		}
		m := hclx.Map(raw)
		svc.Spanner = hclx.Strings(m["spanner_databases"])
		svc.Publishes = hclx.Strings(m["publishes"])
		svc.Pulls = hclx.Strings(m["pulls"])
		svc.Invokes = hclx.Strings(m["invokes"])
		if list, ok := m["buckets"].([]any); ok {
			for _, item := range list {
				if bucket := bucketName(hclx.Field(hclx.Map(item), "bucket")); bucket != "" && !slices.Contains(svc.Buckets, bucket) {
					svc.Buckets = append(svc.Buckets, bucket)
				}
			}
		}
		svc.Sources = append(svc.Sources, at)
	}
}

func (b *builder) hosts(cfg HostsConfig, services map[string]*Service) {
	v, at, err := b.local(cfg.Local)
	if err != nil {
		b.warn("hosts: %v", err)
		return
	}
	lbs := hclx.Map(v)
	for _, lb := range cfg.Select {
		rules := hclx.Map(hclx.Map(lbs[lb])["host_rules"])
		if rules == nil {
			b.warn("hosts: load balancer %q has no host_rules in local.%s", lb, cfg.Local.Local)
			continue
		}
		for _, key := range slices.Sorted(maps.Keys(rules)) {
			rule := hclx.Map(rules[key])
			svc, ok := services[hclx.Field(rule, "service")]
			if !ok {
				continue
			}
			for _, host := range hclx.Strings(rule["hosts"]) {
				if !slices.Contains(svc.Hosts, host) {
					svc.Hosts = append(svc.Hosts, host)
				}
			}
			if !slices.Contains(svc.Sources, at) {
				svc.Sources = append(svc.Sources, at)
			}
		}
	}
}

func (b *builder) push(l Local, services map[string]*Service) {
	v, at, err := b.local(l)
	if err != nil {
		b.warn("push: %v", err)
		return
	}
	for _, topic := range slices.Sorted(maps.Keys(hclx.Map(v))) {
		subs, _ := hclx.Map(hclx.Map(v)[topic])["push_subscriptions"].([]any)
		for _, raw := range subs {
			sub := hclx.Map(raw)
			app := strings.ToLower(hclx.Field(sub, "app_name"))
			svc, ok := services[app]
			if !ok {
				b.warn("push: topic %s pushes to %q, which the services anchor does not list", topic, app)
				continue
			}
			svc.PushFrom = append(svc.PushFrom, Push{Topic: topic, Path: hclx.Field(sub, "path")})
			if !slices.Contains(svc.Sources, at) {
				svc.Sources = append(svc.Sources, at)
			}
		}
	}
}

func (b *builder) releases(cfg Releases, services map[string]*Service) []Package {
	var versions map[string]string
	raw, err := os.ReadFile(filepath.Join(b.root, cfg.Manifest))
	if err == nil {
		err = json.Unmarshal(raw, &versions)
	}
	if err != nil {
		b.warn("releases: %v", err)
		return nil
	}
	var pkgs []Package
	for _, name := range slices.Sorted(maps.Keys(versions)) {
		pkgs = append(pkgs, Package{Name: name, Version: versions[name]})
	}
	for _, svc := range services {
		pkg := cmp.Or(cfg.PackageFor[svc.Repo], svc.Repo)
		if _, ok := versions[pkg]; ok {
			svc.Package = pkg
		}
	}
	return pkgs
}

func (b *builder) agents(path string) []Agent {
	raw, err := os.ReadFile(filepath.Join(b.root, path))
	var parsed map[string]struct {
		Invokers []string `json:"invokers"`
		Durable  bool     `json:"durable"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &parsed)
	}
	if err != nil {
		b.warn("agents: %v", err)
		return nil
	}
	var out []Agent
	for _, name := range slices.Sorted(maps.Keys(parsed)) {
		out = append(out, Agent{Name: name, Invokers: parsed[name].Invokers, Durable: parsed[name].Durable})
	}
	return out
}

func (b *builder) jobs(cfg Jobs) ([]string, []Job) {
	var names []string
	if cfg.Catalogue != "" {
		raw, err := os.ReadFile(filepath.Join(b.root, cfg.Catalogue))
		var catalogue []struct {
			Name string `json:"name"`
		}
		if err == nil {
			err = json.Unmarshal(raw, &catalogue)
		}
		if err != nil {
			b.warn("jobs: %v", err)
		}
		for _, j := range catalogue {
			names = append(names, j.Name)
		}
	}
	var launched []Job
	seen := map[string]bool{}
	for _, p := range cfg.Launched {
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			b.warn("jobs: pattern %q: %v", p.Pattern, err)
			continue
		}
		roots, err := filepath.Glob(filepath.Join(b.root, p.Glob))
		if err != nil || len(roots) == 0 {
			b.warn("jobs: glob %q matches nothing", p.Glob)
		}
		for _, root := range roots {
			launched = append(launched, b.scanLaunched(root, re, seen)...)
		}
	}
	return names, launched
}

func (b *builder) scanLaunched(root string, re *regexp.Regexp, seen map[string]bool) []Job {
	var out []Job
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		switch {
		case err != nil:
			b.warn("jobs: %v", err)
			return nil
		case d.IsDir() && skippedDirs[d.Name()]:
			return filepath.SkipDir
		case d.IsDir() || !sourceExts[filepath.Ext(path)]:
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			b.warn("jobs: %v", err)
			return nil
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if m := re.FindStringSubmatch(line); len(m) > 1 && !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, Job{Name: m[1], Source: fmt.Sprintf("%s:%d", b.rel(path), i+1)})
			}
		}
		return nil
	})
	return out
}

// Build output, dependencies and VCS data hold no declarations worth scanning.
var skippedDirs = map[string]bool{".git": true, "build": true, "node_modules": true, ".gradle": true, "target": true, "dist": true}

var sourceExts = map[string]bool{".scala": true, ".java": true, ".kt": true, ".go": true, ".ts": true, ".py": true}

// linkReverse fills InvokedBy, and SidecarIn: the services that run this service's app image as a
// second container (a web app running the API image beside it, for example).
func linkReverse(services map[string]*Service) {
	sidecarsByImage := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(services)) {
		for _, c := range services[name].Containers {
			if !c.Serving {
				sidecarsByImage[c.Image] = append(sidecarsByImage[c.Image], name)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(services)) {
		svc := services[name]
		for _, target := range svc.Invokes {
			if t, ok := services[target]; ok && !slices.Contains(t.InvokedBy, svc.Name) {
				t.InvokedBy = append(t.InvokedBy, svc.Name)
			}
		}
		for _, c := range svc.Containers {
			if c.Serving {
				for _, host := range sidecarsByImage[c.Image] {
					if host != svc.Name && !slices.Contains(svc.SidecarIn, host) {
						svc.SidecarIn = append(svc.SidecarIn, host)
					}
				}
			}
		}
	}
	for _, svc := range services {
		slices.Sort(svc.InvokedBy)
	}
}

var bucketResource = regexp.MustCompile(`^google_storage_bucket\.\w+\["([^"]+)"\]\.name$`)

// bucketName shortens a reference to a for_each bucket resource to its key, e.g. acme-uploads.
func bucketName(ref string) string {
	if m := bucketResource.FindStringSubmatch(ref); m != nil {
		return m[1] + " (terraform)"
	}
	return ref
}

// SidecarOTelNames lists the OTel service names this service's image reports under when it runs
// as a sidecar elsewhere, keyed by the hosting service.
func (m *Map) SidecarOTelNames(svc Service) map[string]string {
	images := map[string]bool{}
	for _, c := range svc.Containers {
		if c.Serving {
			images[c.Image] = true
		}
	}
	out := map[string]string{}
	for _, host := range m.Services {
		if !slices.Contains(svc.SidecarIn, host.Name) {
			continue
		}
		for _, c := range host.Containers {
			if !c.Serving && images[c.Image] && c.OTelName != "" {
				out[host.Name] = c.OTelName
			}
		}
	}
	return out
}
