// Package health resolves who owns each finding and rolls status up from signals to services to systems.
package health

import (
	"maps"
	"slices"
	"strings"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/report"
	"github.com/adriantr/vitals/internal/source"
	"github.com/adriantr/vitals/internal/topology"
)

// derivedService is the declared service a finding names itself: by service label (any case), OTel
// name, or, with a repository map, a subscription the service pulls.
func derivedService(f report.Finding, cfg *config.Config, m *topology.Map) string {
	if f.Service != "" {
		for _, name := range slices.Sorted(maps.Keys(cfg.Services)) {
			if strings.EqualFold(f.Service, name) || slices.Contains(cfg.Services[name].OTel, f.Service) {
				return name
			}
		}
	}
	kind, sub := f.Ref.Parts()
	if kind != report.RefSubscription || m == nil {
		return ""
	}
	for _, svc := range m.Services {
		if _, declared := cfg.Services[svc.Name]; !declared {
			continue
		}
		if slices.Contains(svc.Pulls, sub) {
			return svc.Name
		}
	}
	return ""
}

// claimed is who the config claims the finding's signal for, by exact name.
func claimed(f report.Finding, cfg *config.Config) string {
	kind, name := f.Ref.Parts()
	switch kind {
	case report.RefPolicy:
		return cfg.Owner(config.ClaimAlert, name)
	case report.RefSLO:
		service, _, _ := strings.Cut(name, "/")
		return cfg.Owner(config.ClaimSLOService, service)
	case report.RefError:
		return cfg.Owner(config.ClaimError, f.Service)
	case report.RefSubscription:
		return cfg.Owner(config.ClaimSubscription, name)
	}
	return ""
}

// Resolve gives every finding an owner: one it already has (declared signals), the declared service
// the signal names itself, or the service or system that claims it by exact name.
func Resolve(reports []report.Report, cfg *config.Config, m *topology.Map) {
	for i := range reports {
		for j := range reports[i].Findings {
			f := &reports[i].Findings[j]
			if f.Owner == "" {
				f.Owner = derivedService(*f, cfg, m)
			}
			if f.Owner == "" {
				f.Owner = claimed(*f, cfg)
			}
			f.OwnerIsService = cfg.IsService(f.Owner)
		}
	}
}

// Rollup computes each service's status from its findings and signals, then each system's from its
// own findings, own signals and its services.
func Rollup(cfg *config.Config, findings []report.Finding, values []report.SignalValue) map[string]report.Status {
	status := map[string]report.Status{}
	for name := range cfg.Services {
		status[name] = report.OK
	}
	for name := range cfg.Systems {
		status[name] = report.OK
	}
	for _, v := range values {
		status[v.Owner] = status[v.Owner].Worse(v.Level)
	}
	for _, f := range findings {
		if _, known := status[f.Owner]; known {
			status[f.Owner] = status[f.Owner].Worse(f.Level)
		}
	}
	for name, sys := range cfg.Systems {
		for _, svc := range sys.Services {
			status[name] = status[name].Worse(status[svc])
		}
	}
	return status
}

// Overview builds one row per declared service from Cloud Run's deployed state and request metrics,
// read by the caller; services with the worst status sort first.
func Overview(cfg *config.Config, w report.Window, run map[string]source.RunService, golden map[string]*source.Golden,
	findings []report.Finding, status map[string]report.Status, values []report.SignalValue) []report.ServiceRow {
	minutes := w.Minutes()
	rows := make([]report.ServiceRow, 0, len(cfg.Services))
	for _, name := range slices.Sorted(maps.Keys(cfg.Services)) {
		svc := cfg.Services[name]
		row := report.ServiceRow{Name: name, Kind: svc.ServiceKind(), Systems: cfg.SystemsOf(name), Status: status[name]}
		if svc.ServiceKind() == config.KindCloudRun {
			if rs, ok := run[name]; ok {
				row.DeployedAt, row.Ready = rs.UpdatedAt, rs.Ready
				if sha := rs.Commit("app"); sha != "" {
					row.Commit = sha[:7]
				}
			} else if len(run) > 0 {
				row.Missing = true
			}
			if g, ok := golden[name]; ok {
				perMin := g.Requests / minutes
				row.PerMinute, row.Instances = &perMin, g.PeakInstances
				if g.Requests > 0 {
					ratio := g.ServerErrors / g.Requests
					row.ErrorRatio, row.P95ms = &ratio, g.P95ms
				}
			}
		}
		for _, f := range findings {
			if f.Owner != name {
				continue
			}
			if f.Level == report.Red {
				row.Red++
			} else {
				row.Amber++
			}
			if f.Chronic {
				row.Chronic++
			}
		}
		for _, v := range values {
			if v.Owner == name && v.Level == report.Unknown {
				row.UnknownSignals++
			}
		}
		rows = append(rows, row)
	}
	slices.SortStableFunc(rows, func(a, b report.ServiceRow) int { return report.WorstFirst(a.Status, b.Status) })
	return rows
}

// Systems builds one row per declared system, plus "unowned" for findings nobody owns.
func Systems(cfg *config.Config, findings []report.Finding, status map[string]report.Status) []report.SystemRow {
	var rows []report.SystemRow
	for _, name := range slices.Sorted(maps.Keys(cfg.Systems)) {
		sys := cfg.Systems[name]
		row := report.SystemRow{Name: name, Description: sys.Description, Status: status[name], Services: len(sys.Services)}
		for _, svc := range sys.Services {
			if st := status[svc]; st.Breach() {
				row.Degraded = append(row.Degraded, svc+" "+string(st))
			}
		}
		for _, f := range findings {
			if f.Owner == name {
				row.Own++
			}
		}
		rows = append(rows, row)
	}
	unowned := 0
	for _, f := range findings {
		if f.Owner == "" {
			unowned++
		}
	}
	if unowned > 0 {
		rows = append(rows, report.SystemRow{Name: "unowned", Status: report.Amber, Own: unowned, Description: "findings no declared service or system claims"})
	}
	slices.SortStableFunc(rows, func(a, b report.SystemRow) int { return report.WorstFirst(a.Status, b.Status) })
	return rows
}
