package source

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/adriantr/vitals/internal/report"
)

type Commit struct {
	SHA     string     `json:"sha"`
	Subject string     `json:"subject"`
	Date    *time.Time `json:"date,omitempty"`
	URL     string     `json:"url"`
}

// LookupCommit resolves a deployed image's commit SHA on GitHub.
func LookupCommit(ctx context.Context, s Scope, t *report.Tracker, sha string) *Commit {
	gh := s.Environment.GitHub
	if gh == nil || sha == "" {
		return nil
	}
	var c struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
		Commit  struct {
			Message   string `json:"message"`
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	u := endpoint(fmt.Sprintf("https://api.github.com/repos/%s/commits/%s", gh.Repo, url.PathEscape(sha)), nil)
	if !t.JSON(ctx, "commit "+sha[:7], u, nil, s.GitHub, &c) {
		return nil
	}
	return &Commit{SHA: c.SHA, Subject: report.FirstLine(c.Commit.Message, 120), Date: parseTime(c.Commit.Committer.Date), URL: c.HTMLURL}
}

type Release struct {
	Tag  string     `json:"tag"`
	Date *time.Time `json:"date,omitempty"`
	URL  string     `json:"url"`
}

const releasesPerPage = 100

type githubRelease struct {
	TagName     string `json:"tag_name"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
	Draft       bool   `json:"draft"`
}

// releasePage fetches one page (1-based) of releases, newest first; ok is false when the read failed.
type releasePage func(page int) (list []githubRelease, ok bool)

func githubReleasePages(ctx context.Context, s Scope, t *report.Tracker) releasePage {
	gh := s.Environment.GitHub
	return func(page int) ([]githubRelease, bool) {
		var list []githubRelease
		u := endpoint(fmt.Sprintf("https://api.github.com/repos/%s/releases", gh.Repo),
			url.Values{"per_page": {strconv.Itoa(releasesPerPage)}, "page": {strconv.Itoa(page)}})
		return list, t.JSON(ctx, "releases", u, nil, s.GitHub, &list)
	}
}

// lastReleasePage reports whether no later page can hold a release published at or after since.
func lastReleasePage(list []githubRelease, since time.Time) bool {
	if len(list) < releasesPerPage {
		return true
	}
	oldest := parseTime(list[len(list)-1].PublishedAt)
	return oldest != nil && oldest.Before(since)
}

// listReleases reads every release newest first until one is older than since; a listing cut short is a failure.
func listReleases(t *report.Tracker, fetch releasePage, since time.Time) []githubRelease {
	return paged(t, "releases", func(token string) ([]githubRelease, string, bool) {
		page := 1
		if token != "" {
			page, _ = strconv.Atoi(token)
		}
		list, ok := fetch(page)
		if !ok {
			return nil, "", false
		}
		if lastReleasePage(list, since) {
			return list, "", true
		}
		return list, strconv.Itoa(page + 1), true
	})
}

// LatestReleases returns the newest GitHub release per tag component ("api" for "api-v25.42.0") published in the last 90 days.
func LatestReleases(ctx context.Context, s Scope, t *report.Tracker) map[string]Release {
	if s.Environment.GitHub == nil {
		return nil
	}
	return latestReleases(githubReleasePages(ctx, s, t), s.Window.End.AddDate(0, 0, -90))
}

// latestReleases stops at the first page whose releases name no new component, or after maxPages. Stopping early
// is not a failure: a component whose newest release lies further back shows no release.
func latestReleases(fetch releasePage, since time.Time) map[string]Release {
	out := map[string]Release{}
	for page := 1; page <= maxPages; page++ {
		list, ok := fetch(page)
		if !ok {
			break
		}
		named, added := false, false
		for _, r := range list {
			i := strings.LastIndex(r.TagName, "-v")
			at := parseTime(r.PublishedAt)
			if r.Draft || i <= 0 || (at != nil && at.Before(since)) {
				continue
			}
			named = true
			if component := r.TagName[:i]; out[component].Tag == "" {
				out[component] = Release{Tag: r.TagName, Date: at, URL: r.HTMLURL}
				added = true
			}
		}
		if named && !added || lastReleasePage(list, since) {
			break
		}
	}
	return out
}
