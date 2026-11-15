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

// listReleases reads releases newest first until one is older than since.
func listReleases(ctx context.Context, s Scope, t *report.Tracker, since time.Time) []githubRelease {
	gh := s.Environment.GitHub
	return paged(t, "releases", func(token string) ([]githubRelease, string, bool) {
		page := 1
		if token != "" {
			page, _ = strconv.Atoi(token)
		}
		var list []githubRelease
		u := endpoint(fmt.Sprintf("https://api.github.com/repos/%s/releases", gh.Repo),
			url.Values{"per_page": {strconv.Itoa(releasesPerPage)}, "page": {strconv.Itoa(page)}})
		if !t.JSON(ctx, "releases", u, nil, s.GitHub, &list) {
			return nil, "", false
		}
		if len(list) < releasesPerPage {
			return list, "", true
		}
		if oldest := parseTime(list[len(list)-1].PublishedAt); oldest != nil && oldest.Before(since) {
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
	out := map[string]Release{}
	for _, r := range listReleases(ctx, s, t, s.Window.End.AddDate(0, 0, -90)) {
		i := strings.LastIndex(r.TagName, "-v")
		if r.Draft || i <= 0 {
			continue
		}
		component := r.TagName[:i]
		if _, seen := out[component]; !seen {
			out[component] = Release{Tag: r.TagName, Date: parseTime(r.PublishedAt), URL: r.HTMLURL}
		}
	}
	return out
}
