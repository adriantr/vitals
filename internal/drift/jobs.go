package drift

import (
	"fmt"
	"maps"
	"slices"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/source"
)

// jobDrift is what check finds between the declared jobs and the job projects.
type jobDrift struct {
	undeclared   []source.LiveJob // live jobs matching jobs.defaults.match that are not declared
	missing      []string         // declared jobs their project does not have
	unscheduled  []string         // schedulers that start an undeclared job
	matchSkipped bool             // jobs.defaults.match is unset, so undeclared jobs are not looked for
}

// findJobDrift compares the declared jobs with what ReadJobs listed; est.Live must be read.
func findJobDrift(cfg *config.Config, est source.JobEstate) jobDrift {
	var d jobDrift
	declared := map[string]bool{}
	for _, st := range est.States {
		j := st.Job
		key := string(j.Kind) + "/" + string(j.Project) + "/" + j.Name
		declared[key] = true
		if _, ok := est.Live[j.Kind][string(j.Project)+"/"+j.Name]; !ok {
			d.missing = append(d.missing, fmt.Sprintf("%s (%s in %s)", j.Name, j.Kind, j.Project))
		}
	}
	match := cfg.JobMatch()
	d.matchSkipped = match == nil
	for _, kind := range []config.JobKind{config.JobCloudRun, config.JobWorkflow} {
		live := est.Live[kind]
		for _, key := range slices.Sorted(maps.Keys(live)) {
			j := live[key]
			if match != nil && match.MatchString(j.Name) && !declared[string(kind)+"/"+key] {
				d.undeclared = append(d.undeclared, j)
			}
		}
	}
	for _, sc := range est.Schedules {
		t := sc.Target
		if !declared[string(t.Kind)+"/"+string(t.Project)+"/"+t.Name] {
			d.unscheduled = append(d.unscheduled, fmt.Sprintf("%s starts %s %s in %s", sc.Name, t.Kind, t.Name, t.Project))
		}
	}
	slices.Sort(d.missing)
	slices.Sort(d.unscheduled)
	return d
}

func liveJobItems(jobs []source.LiveJob) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, fmt.Sprintf("%s (%s in %s)", j.Name, j.Kind, j.Project))
	}
	return out
}
