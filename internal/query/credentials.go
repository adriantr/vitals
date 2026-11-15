package query

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"golang.org/x/oauth2/google"
)

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// Bearer is a fixed token, e.g. an API key read from the environment.
type Bearer string

func (b Bearer) Token(context.Context) (string, error) { return string(b), nil }

// cached runs fetch once and reuses its answer, error included, for the life of the process.
type cached struct {
	once  sync.Once
	fetch func(ctx context.Context) (string, error)
	token string
	err   error
}

func (c *cached) Token(ctx context.Context) (string, error) {
	c.once.Do(func() { c.token, c.err = c.fetch(ctx) })
	return c.token, c.err
}

// Google prefers ADC when GOOGLE_APPLICATION_CREDENTIALS is set (CI), else the gcloud login so reads
// run as the person at the keyboard, else ADC (metadata server).
func Google() Credential {
	return &cached{fetch: func(ctx context.Context) (string, error) {
		if _, err := exec.LookPath("gcloud"); err == nil && os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" {
			return command(ctx, "gcloud", "auth", "print-access-token")
		}
		source, err := google.DefaultTokenSource(ctx, cloudPlatformScope)
		if err != nil {
			return "", fmt.Errorf("no gcloud on PATH and no Application Default Credentials: %w", err)
		}
		token, err := source.Token()
		if err != nil {
			return "", err
		}
		return token.AccessToken, nil
	}}
}

// GitHub reads GITHUB_TOKEN or GH_TOKEN, then falls back to the gh CLI's login.
func GitHub() Credential {
	return &cached{fetch: func(ctx context.Context) (string, error) {
		for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
			if token := os.Getenv(name); token != "" {
				return token, nil
			}
		}
		if _, err := exec.LookPath("gh"); err != nil {
			return "", errors.New("set GITHUB_TOKEN or log in with the gh CLI")
		}
		return command(ctx, "gh", "auth", "token")
	}}
}

func command(ctx context.Context, name string, args ...string) (string, error) {
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, lastLine(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return lines[len(lines)-1]
}
