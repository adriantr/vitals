package source

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/adriantr/vitals/internal/report"
)

const pubsubSpan = 10 * time.Minute

type subscriptionValue struct {
	subscription string
	value        float64
}

// SubscriptionFilter selects one Pub/Sub metric; match narrows it to subscriptions whose id fully matches.
func SubscriptionFilter(metric, match string) string {
	filter := fmt.Sprintf(`metric.type="pubsub.googleapis.com/subscription/%s"`, metric)
	if match == "" {
		return filter
	}
	return fmt.Sprintf(`%s AND resource.labels.subscription_id=monitoring.regex.full_match(%q)`, filter, match)
}

func latestPerSubscription(ctx context.Context, s Scope, t *report.Tracker, metric, match string) []subscriptionValue {
	w := report.Window{Start: s.Window.End.Add(-pubsubSpan), End: s.Window.End}
	extra := url.Values{
		"aggregation.alignmentPeriod":  {fmt.Sprintf("%ds", int(pubsubSpan.Seconds()))},
		"aggregation.perSeriesAligner": {"ALIGN_MAX"},
	}
	list, ok := querySeries(ctx, s, t, metric, SubscriptionFilter(metric, match), w, extra)
	if !ok {
		return nil
	}
	var out []subscriptionValue
	for _, series := range list.TimeSeries {
		points := series.readings()
		subscription := series.Resource.Labels["subscription_id"]
		if subscription == "" || len(points) == 0 {
			continue
		}
		out = append(out, subscriptionValue{subscription, points[len(points)-1].Value})
	}
	return out
}

func subscriptionFinding(s Scope, level report.Status, subscription, detail string) report.Finding {
	return report.Finding{
		Level: level, Source: "pubsub",
		Title: "Pub/Sub " + subscription, Detail: detail, Ref: report.NewRef(report.RefSubscription, subscription),
	}
}

func PubSub(ctx context.Context, s Scope) report.Report {
	t := report.NewTracker("pubsub", s.Client)
	th := s.Config.Thresholds
	deadLetter := regexp.MustCompile("^(?:" + th.DeadLetterMatch + ")$")
	var dlqs, lags []subscriptionValue
	Concurrently(
		func() { dlqs = latestPerSubscription(ctx, s, t, "num_undelivered_messages", th.DeadLetterMatch) },
		// RE2 has no negative lookahead, so dead-letter subscriptions are dropped below, not in the filter.
		func() { lags = latestPerSubscription(ctx, s, t, "oldest_unacked_message_age", "") },
	)

	var findings []report.Finding
	for _, d := range dlqs {
		if d.value > 0 {
			findings = append(findings, subscriptionFinding(s, report.Amber, d.subscription, fmt.Sprintf("dead-letter backlog of %.0f message(s)", d.value)))
		}
	}
	for _, l := range lags {
		age := time.Duration(l.value) * time.Second
		if deadLetter.MatchString(l.subscription) || age < th.PubSubLagAmber {
			continue
		}
		level := report.Amber
		if age >= th.PubSubLagRed {
			level = report.Red
		}
		findings = append(findings, subscriptionFinding(s, level, l.subscription, fmt.Sprintf("oldest unacked message is %s old", age.Round(time.Minute))))
	}
	return t.Report(len(lags), "subscriptions", findings, nil, nil)
}
