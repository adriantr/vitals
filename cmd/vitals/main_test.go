package main

import (
	"errors"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	t.Setenv("VITALS_CONFIG", "")
	opts, err := parse([]string{"component", "--since", "2d", "checkout", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.ref != "checkout" || opts.since != 48*time.Hour || !opts.json || opts.config != "vitals.yaml" {
		t.Errorf("opts = %+v", opts)
	}
	opts, err = parse([]string{"snapshot", "--at", "2026-01-02T03:04:05Z", "--slo"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.at.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) || !opts.slo || opts.since != 6*time.Hour {
		t.Errorf("opts = %+v", opts)
	}
	t.Setenv("VITALS_CONFIG", "/etc/vitals.yaml")
	if opts, _ := parse([]string{"check"}); opts.config != "/etc/vitals.yaml" {
		t.Errorf("config = %q, want $VITALS_CONFIG", opts.config)
	}
}

func TestParseRejects(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"status"},
		{"explain"},
		{"component"},
		{"snapshot", "extra"},
		{"snapshot", "--since", "8d"},
		{"snapshot", "--since", "30s"},
		{"snapshot", "--at", "yesterday"},
		{"snapshot", "--at", "2999-01-01T00:00:00Z"},
		{"check", "--slo"},
	} {
		if _, err := parse(args); !errors.Is(err, errUsage) {
			t.Errorf("parse(%q) = %v, want a usage error", args, err)
		}
	}
}
