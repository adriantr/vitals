package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/adriantr/vitals/internal/config"
)

const (
	maxChanges = 25
	maxNotes   = 15
)

func Stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04Z") }

func findingLine(f Finding) string {
	var b strings.Builder
	owner := f.Owner
	if owner == "" {
		owner = "unowned"
	}
	b.WriteString("- ")
	if f.Chronic {
		b.WriteString(statusMark[f.Level] + " · ")
	}
	fmt.Fprintf(&b, "**%s** [%s · %s]", f.Title, f.Source, owner)
	if f.Since != nil {
		fmt.Fprintf(&b, " · since %s", Stamp(*f.Since))
	}
	fmt.Fprintf(&b, " — %s", f.Detail)
	if f.OwnerIsService {
		fmt.Fprintf(&b, " · `vitals component %s`", f.Owner)
	}
	fmt.Fprintf(&b, " · ref `%s`", f.Ref)
	for _, l := range f.Samples {
		b.WriteString("\n" + SampleLine(l))
	}
	return b.String()
}

func changeLine(c Change) string {
	line := fmt.Sprintf("- %s %s %s", Stamp(c.At), c.Kind, c.What)
	if c.By != "" {
		line += " by " + c.By
	}
	return line
}

func section(title string, lines []string, limit int) []string {
	if len(lines) == 0 {
		return nil
	}
	out := []string{"", "## " + title}
	if len(lines) > limit {
		out = append(out, lines[:limit]...)
		return append(out, fmt.Sprintf("- … %d more (use --json)", len(lines)-limit))
	}
	return append(out, lines...)
}

func RenderChanges(changes []Change) []string {
	if len(changes) == 0 {
		return []string{"", "## Changes (0)", "- no deploys or releases in the window"}
	}
	lines := make([]string, 0, len(changes))
	for _, c := range changes {
		lines = append(lines, changeLine(c))
	}
	return section(fmt.Sprintf("Changes (%d)", len(changes)), lines, maxChanges)
}

func RenderBlindSpots(reports []Report) []string {
	var lines []string
	for _, r := range reports {
		for _, p := range r.Problems {
			lines = append(lines, fmt.Sprintf("- [%s %s] %s", r.Source, r.Status, p))
		}
	}
	return section("Blind spots", lines, len(lines))
}

var statusMark = map[Status]string{Red: "RED", Amber: "amber", Unknown: "unknown", OK: "ok"}

func renderSystems(rows []SystemRow) []string {
	if len(rows) == 0 {
		return nil
	}
	lines := []string{"", fmt.Sprintf("## Systems (%d)", len(rows)), "| system | status | services | degraded services | own findings |", "| --- | --- | --- | --- | --- |"}
	for _, r := range rows {
		degraded := strings.Join(r.Degraded, ", ")
		if degraded == "" {
			degraded = "—"
		}
		name := r.Name
		if r.Description != "" {
			name += " — " + r.Description
		}
		lines = append(lines, fmt.Sprintf("| %s | %s | %d | %s | %d |", name, statusMark[r.Status], r.Services, degraded, r.Own))
	}
	return lines
}

func renderServices(rows []ServiceRow) []string {
	if len(rows) == 0 {
		return nil
	}
	lines := []string{"", fmt.Sprintf("## Services (%d)", len(rows)), "| service | status | systems | deployed | commit | req/min | 5xx | p95 | inst | findings |", "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |"}
	for _, r := range rows {
		deployed := "—"
		switch {
		case r.Kind != config.KindCloudRun:
			deployed = string(r.Kind)
		case r.Missing:
			deployed = "not in Cloud Run"
		case r.DeployedAt != nil:
			deployed = Stamp(*r.DeployedAt)
			if !r.Ready {
				deployed += " NOT READY"
			}
		}
		var findings []string
		if r.Red > 0 {
			findings = append(findings, fmt.Sprintf("%d red", r.Red))
		}
		if r.Amber > 0 {
			findings = append(findings, fmt.Sprintf("%d amber", r.Amber))
		}
		if r.Chronic > 0 {
			findings = append(findings, fmt.Sprintf("%d of them chronic", r.Chronic))
		}
		if r.UnknownSignals > 0 {
			findings = append(findings, fmt.Sprintf("%d signal(s) no data", r.UnknownSignals))
		}
		lines = append(lines, fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |",
			r.Name, statusMark[r.Status], strings.Join(r.Systems, ", "), deployed, r.Commit,
			Optional(r.PerMinute, ""), Optional(r.ErrorRatio, "percent"), Optional(r.P95ms, "ms"), Optional(r.Instances, ""), strings.Join(findings, ", ")))
	}
	return lines
}

func RenderSnapshot(s Snapshot) string {
	var red, amber, chronic []string
	for _, f := range s.Findings {
		switch {
		case f.Chronic:
			chronic = append(chronic, findingLine(f))
		case f.Level == Red:
			red = append(red, findingLine(f))
		default:
			amber = append(amber, findingLine(f))
		}
	}
	status := strings.ToUpper(string(s.Overall))
	if len(s.Blind) > 0 {
		status += fmt.Sprintf(" (partial: %s unknown)", strings.Join(s.Blind, ", "))
	}
	sources := make([]string, 0, len(s.Sources))
	var notes []string
	for _, r := range s.Sources {
		sources = append(sources, fmt.Sprintf("%s %s (%d %s)", r.Source, r.Status, r.Checked, r.CheckedWhat))
		for _, n := range r.Notes {
			notes = append(notes, fmt.Sprintf("- [%s] %s", r.Source, n))
		}
	}
	lines := []string{
		fmt.Sprintf("# System health — %s — %s", s.Env, status),
		fmt.Sprintf("window %s → %s · %d red, %d amber, %d chronic", Stamp(s.Window.Start), Stamp(s.Window.End), len(red), len(amber), len(chronic)),
		"sources: " + strings.Join(sources, " · "),
	}
	lines = append(lines, renderSystems(s.Systems)...)
	lines = append(lines, renderServices(s.Services)...)
	if s.Historical && len(s.Services) > 0 {
		lines = append(lines, "", "_deployed and commit are blank for a past window; its deploys are under Changes_")
	}
	lines = append(lines, section(fmt.Sprintf("Red (%d)", len(red)), red, len(red))...)
	lines = append(lines, section(fmt.Sprintf("Amber (%d)", len(amber)), amber, len(amber))...)
	lines = append(lines, section(fmt.Sprintf("Chronic (%d) — held before this window; counted in status", len(chronic)), chronic, len(chronic))...)
	lines = append(lines, RenderChanges(s.Changes)...)
	lines = append(lines, section("Notes", notes, maxNotes)...)
	lines = append(lines, RenderBlindSpots(s.Sources)...)
	return strings.Join(lines, "\n")
}
