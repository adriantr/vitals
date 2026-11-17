package source

import (
	"fmt"
	"testing"
	"time"

	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
)

var releaseNow = time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

// fakeReleases serves full pages of releases, one per hour going back from releaseNow, tagged by tag(i)
// for the i-th newest release, and counts the pages read.
type fakeReleases struct {
	tag   func(i int) string
	reads int
}

func (f *fakeReleases) page(page int) ([]githubRelease, bool) {
	f.reads++
	list := make([]githubRelease, releasesPerPage)
	for j := range list {
		i := (page-1)*releasesPerPage + j
		list[j] = githubRelease{
			TagName:     f.tag(i),
			PublishedAt: releaseNow.Add(-time.Duration(i) * time.Hour).Format(time.RFC3339),
		}
	}
	return list, true
}

func TestLatestReleasesStopsWhenAPageAddsNoComponent(t *testing.T) {
	// Page 1 holds "api" and "web"; page 2 adds "worker"; page 3 repeats "api".
	f := &fakeReleases{tag: func(i int) string {
		switch {
		case i < releasesPerPage:
			return []string{"api", "web"}[i%2] + fmt.Sprintf("-v1.%d.0", 1000-i)
		case i < 2*releasesPerPage:
			return fmt.Sprintf("worker-v2.%d.0", 1000-i)
		default:
			return fmt.Sprintf("api-v0.%d.0", 1000-i)
		}
	}}
	got := latestReleases(f.page, releaseNow.AddDate(0, 0, -90))
	if f.reads != 3 {
		t.Errorf("read %d pages, want 3", f.reads)
	}
	want := map[string]string{"api": "api-v1.1000.0", "web": "web-v1.999.0", "worker": "worker-v2.900.0"}
	if len(got) != len(want) {
		t.Errorf("got %d components, want %d: %v", len(got), len(want), got)
	}
	for c, tag := range want {
		if got[c].Tag != tag {
			t.Errorf("%s: tag %q, want %q", c, got[c].Tag, tag)
		}
	}
}

func TestLatestReleasesStopsAtThePageCap(t *testing.T) {
	// Every release is a new component, so only the cap ends the listing.
	f := &fakeReleases{tag: func(i int) string { return fmt.Sprintf("c%d-v1.0.0", i) }}
	got := latestReleases(f.page, releaseNow.AddDate(-1, 0, 0))
	if f.reads != maxPages || len(got) != maxPages*releasesPerPage {
		t.Errorf("read %d pages for %d components, want %d pages for %d", f.reads, len(got), maxPages, maxPages*releasesPerPage)
	}
}

func TestLatestReleasesSkipsDraftsUntaggedAndOld(t *testing.T) {
	list := []githubRelease{
		{TagName: "api-v2.0.0", Draft: true, PublishedAt: releaseNow.Format(time.RFC3339)},
		{TagName: "nightly", PublishedAt: releaseNow.Format(time.RFC3339)},
		{TagName: "api-v1.0.0", PublishedAt: releaseNow.Add(-time.Hour).Format(time.RFC3339)},
		{TagName: "web-v1.0.0", PublishedAt: releaseNow.AddDate(0, 0, -100).Format(time.RFC3339)},
	}
	got := latestReleases(func(int) ([]githubRelease, bool) { return list, true }, releaseNow.AddDate(0, 0, -90))
	if len(got) != 1 || got["api"].Tag != "api-v1.0.0" {
		t.Errorf("got %v, want only api-v1.0.0", got)
	}
}

func TestListReleasesFailsWhenCutShort(t *testing.T) {
	f := &fakeReleases{tag: func(i int) string { return fmt.Sprintf("api-v1.%d.0", i) }}
	tr := report.NewTracker("test", nil)
	listReleases(tr, f.page, releaseNow.AddDate(-1, 0, 0))
	if tr.Status() != query.Failed {
		t.Errorf("status %v after %d pages, want %v", tr.Status(), f.reads, query.Failed)
	}
}
