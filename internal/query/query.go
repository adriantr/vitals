// Package query runs the HTTP reads every source makes: bounded concurrency, retries on quota
// errors, and a status that tells a missing grant (BLOCKED) from a broken call (FAILED).
package query

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Status string

const (
	OK      Status = "OK"
	Blocked Status = "BLOCKED"
	Failed  Status = "FAILED"
	// Skipped: the source cannot answer for this window (Error Reporting for a past window).
	Skipped Status = "SKIPPED"
)

const (
	concurrency   = 8
	timeout       = 2 * time.Minute
	errorBodyMax  = 2000
	userAgentName = "vitals"
)

// Cloud Monitoring answers bursts with 429 under a per-minute quota; the retries span into the next minute.
var retryDelays = []time.Duration{5 * time.Second, 20 * time.Second, 40 * time.Second}

// Error is a read that did not produce a usable answer.
type Error struct {
	Status  Status
	Exit    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Exit, e.Message) }

// Credential yields the bearer token for one API.
type Credential interface {
	Token(ctx context.Context) (string, error)
}

type Client struct {
	http  *http.Client
	slots chan struct{}
	log   func(string)
}

func New(log func(string)) *Client {
	return &Client{http: &http.Client{Timeout: timeout}, slots: make(chan struct{}, concurrency), log: log}
}

// JSON GETs u, or POSTs body as JSON when it is non-nil, and decodes the answer into out.
func (c *Client) JSON(ctx context.Context, title string, u *url.URL, body any, cred Credential, out any) error {
	started := time.Now()
	err := c.fetch(ctx, u, body, cred, out)
	status, exit := OK, "ok"
	var qe *Error
	if errors.As(err, &qe) {
		status, exit = qe.Status, qe.Exit
	}
	c.log(fmt.Sprintf("%-7s %s (%s, %dms)", status, title, exit, time.Since(started).Milliseconds()))
	return err
}

func (c *Client) fetch(ctx context.Context, u *url.URL, body any, cred Credential, out any) error {
	token, err := cred.Token(ctx)
	if err != nil {
		return &Error{Status: Blocked, Exit: "no credentials", Message: err.Error()}
	}
	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return &Error{Status: Failed, Exit: "unencodable body", Message: err.Error()}
		}
	}
	for attempt := 0; ; attempt++ {
		resp, err := c.once(ctx, u, payload, token)
		if err != nil {
			return &Error{Status: Failed, Exit: "no response", Message: err.Error()}
		}
		delay, retry := retryDelay(resp, attempt)
		if !retry {
			return decode(resp, out)
		}
		_ = resp.Body.Close()
		select {
		case <-ctx.Done():
			return &Error{Status: Failed, Exit: "cancelled", Message: ctx.Err().Error()}
		case <-time.After(delay):
		}
	}
}

func (c *Client) once(ctx context.Context, u *url.URL, payload []byte, token string) (*http.Response, error) {
	c.slots <- struct{}{}
	defer func() { <-c.slots }()
	method, reader := http.MethodGet, io.Reader(nil)
	if payload != nil {
		method, reader = http.MethodPost, bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgentName)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

func retryDelay(resp *http.Response, attempt int) (time.Duration, bool) {
	if (resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable) ||
		attempt >= len(retryDelays) {
		return 0, false
	}
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second, true
	}
	return retryDelays[attempt], true
}

func decode(resp *http.Response, out any) error {
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return &Error{Status: Failed, Exit: fmt.Sprintf("HTTP %d", resp.StatusCode), Message: err.Error()}
	}
	if resp.StatusCode >= 300 {
		status := Failed
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			status = Blocked
		}
		return &Error{Status: status, Exit: fmt.Sprintf("HTTP %d", resp.StatusCode), Message: apiMessage(raw)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &Error{Status: Failed, Exit: fmt.Sprintf("HTTP %d, unparseable", resp.StatusCode), Message: err.Error()}
	}
	return nil
}

// Google wraps the reason in {"error": {"message": …}} and GitHub in {"message": …}.
func apiMessage(raw []byte) string {
	var google struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &google) == nil {
		if google.Error.Message != "" {
			return google.Error.Message
		}
		if google.Message != "" {
			return google.Message
		}
	}
	text := strings.TrimSpace(string(raw))
	if len(text) > errorBodyMax {
		text = text[:errorBodyMax]
	}
	return text
}
