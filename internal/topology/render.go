package topology

import (
	"fmt"
	"slices"
	"strings"

	"github.com/adriantr/vitals/internal/text"
)

func line(label, value string) string { return fmt.Sprintf("%-9s %s", label, value) }

// Card describes one service as the repository declares it, one fact per line.
func Card(s Service) []string {
	out := []string{line("repo", s.Repo+pkgSuffix(s))}
	if s.Manifest != "" {
		out = append(out, line("manifest", s.Manifest))
	}
	var run []string
	for _, v := range []string{s.Ingress, s.Scaling} {
		if v != "" {
			run = append(run, v)
		}
	}
	if s.ServiceAccount != "" {
		run = append(run, "SA "+s.ServiceAccount)
	}
	if len(run) > 0 {
		out = append(out, line("runs", strings.Join(run, " · ")))
	}
	for _, c := range s.Containers {
		facts := []string{"image " + c.Image}
		if c.OTelName != "" {
			facts = append(facts, "otel "+c.OTelName)
		}
		for _, e := range c.Env {
			facts = append(facts, e.Label+" "+e.Value)
		}
		role := "sidecar"
		if c.Serving {
			role = "serving"
		}
		out = append(out, line("container", c.Name+" ("+role+"): "+strings.Join(facts, " · ")))
		if len(c.Calls) > 0 {
			out = append(out, line("calls", text.List(c.Calls)))
		}
		if len(c.Secrets) > 0 {
			out = append(out, line("secrets", text.List(c.Secrets)))
		}
	}
	if len(s.Hosts) > 0 {
		out = append(out, line("serves", text.List(s.Hosts)))
	}
	if len(s.Spanner) > 0 {
		out = append(out, line("spanner", text.List(s.Spanner)))
	}
	if len(s.Buckets) > 0 {
		out = append(out, line("buckets", text.List(s.Buckets)))
	}
	if len(s.Publishes) > 0 {
		out = append(out, line("publishes", text.List(s.Publishes)))
	}
	if len(s.Pulls) > 0 {
		out = append(out, line("pulls", text.List(s.Pulls)))
	}
	if len(s.PushFrom) > 0 {
		var push []string
		for _, p := range s.PushFrom {
			push = append(push, strings.TrimSuffix(p.Topic+" → /"+p.Path, " → /"))
		}
		out = append(out, line("push from", text.List(push)))
	}
	if len(s.Invokes) > 0 {
		out = append(out, line("invokes", text.List(s.Invokes)))
	}
	if len(s.InvokedBy) > 0 {
		out = append(out, line("called by", text.List(s.InvokedBy)))
	}
	if len(s.SidecarIn) > 0 {
		out = append(out, line("sidecar in", text.List(s.SidecarIn)+" (same image)"))
	}
	out = append(out, line("sources", text.List(s.Sources)))
	return out
}

func pkgSuffix(s Service) string {
	if s.Package == "" {
		return ""
	}
	return " · release package " + s.Package
}

// Render prints the whole map: an overview, then one card per service.
func Render(m *Map) string {
	byRepo := map[string][]Service{}
	for _, s := range m.Services {
		byRepo[s.Repo] = append(byRepo[s.Repo], s)
	}
	repos := make([]string, 0, len(byRepo))
	for r := range byRepo {
		repos = append(repos, r)
	}
	slices.Sort(repos)

	lines := []string{
		fmt.Sprintf("# System map — %s", m.Env),
		fmt.Sprintf("%d services · %d agents · %d batch jobs · built %s from the repository", len(m.Services), len(m.Agents), len(m.BatchJobs), m.GeneratedAt.Format("2006-01-02 15:04Z")),
		"",
		"## Overview",
	}
	for _, r := range repos {
		var names []string
		for _, s := range byRepo[r] {
			entry := s.Name
			if len(s.Hosts) > 0 {
				entry += " (" + strings.Join(s.Hosts, ", ") + ")"
			}
			names = append(names, entry)
		}
		lines = append(lines, fmt.Sprintf("- **%s**: %s", r, strings.Join(names, ", ")))
	}
	if len(m.Packages) > 0 {
		var pkgs []string
		for _, p := range m.Packages {
			pkgs = append(pkgs, p.Name+" "+p.Version)
		}
		lines = append(lines, "- **release versions in the repo**: "+strings.Join(pkgs, ", "))
	}
	if len(m.Agents) > 0 {
		var agents []string
		for _, a := range m.Agents {
			agents = append(agents, a.Name)
		}
		lines = append(lines, "- **agents** (Vertex Agent Engine): "+strings.Join(agents, ", "))
	}
	if len(m.Launched) > 0 {
		var jobs []string
		for _, j := range m.Launched {
			jobs = append(jobs, j.Name)
		}
		lines = append(lines, "- **Cloud Run jobs launched from code**: "+strings.Join(jobs, ", "))
	}
	for _, s := range m.Services {
		lines = append(lines, "", fmt.Sprintf("## %s", s.Name), "```")
		lines = append(lines, Card(s)...)
		lines = append(lines, "```")
	}
	if len(m.Warnings) > 0 {
		lines = append(lines, "", "## Unresolved")
		for _, w := range m.Warnings {
			lines = append(lines, "- "+w)
		}
	}
	return strings.Join(lines, "\n")
}
