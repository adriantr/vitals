# Writing signals

A signal is a PromQL query plus thresholds. vitals runs it as an instant query at the end of the
window against Cloud Monitoring's Prometheus-compatible API:

```
GET https://monitoring.googleapis.com/v1/projects/<gcp.project>/location/global/prometheus/api/v1/query
```

Any metric Cloud Monitoring holds is available: Google Cloud system metrics, OpenTelemetry metrics
sent through OTLP, log-based metrics and Managed Service for Prometheus. The query sees what the
project's metrics scope sees, so add other projects to that scope if a signal needs their metrics.

Try every query in the Metrics Explorer **PromQL** tab before you add it, then run `vitals check`,
which lists signals that returned no data.

## Cloud Monitoring PromQL in five rules

1. **Metric names use underscores and a colon.** Replace the first `/` of the metric type with `:`
   and every `.` and remaining `/` with `_`.
   `run.googleapis.com/request_count` becomes `run_googleapis_com:request_count`.
   `workload.googleapis.com/http.server.request.duration` becomes
   `workload_googleapis_com:http_server_request_duration`.
2. **Pin the resource with `monitored_resource`.** Many metrics exist on several resource types.
   `{monitored_resource="cloud_run_revision"}` keeps a query from mixing them or from matching
   nothing. Resource labels (`service_name`, `instance_id`, `topic_id`) and metric labels are both
   plain labels.
3. **Distributions are histograms.** Add `_bucket` and use `histogram_quantile` over `le`:
   `histogram_quantile(0.95, sum by (le) (rate(run_googleapis_com:request_latencies_bucket{...}[{window}])))`.
4. **Ratios need a zero for the numerator.** When nothing failed the 5xx series does not exist, and
   `nothing / total` is no data, not 0. Write `(sum(rate(errors[{window}])) or vector(0)) / sum(rate(total[{window}]))`.
   With `by`, `vector(0)` has no labels; use `or (0 * sum by (label) (rate(total[...])))` instead.
5. **Use `{window}` as the range.** It becomes the `--since` window (`6h`), so the signal measures
   the same period as the rest of the report. Use a fixed range (`[24h]`) only when the signal is
   about a fixed period, such as "jobs that failed today".

## Choosing thresholds and `noData`

- Compare with the past when the absolute level varies by service: divide by the same query with
  `offset 1d` or `offset 7d` (same weekday). Amber at 2× and red at 4× are reasonable starts.
- Add a floor so tiny baselines do not produce huge ratios:
  `(ratio) and (current > 1)`.
- Use `below: true` for things that must not drop, such as messages published or uptime.
- Set `noData: ok` when silence means healthy (no errors, no failed probes, no traffic). Leave it at
  `unknown` when silence means the metric or the thing that emits it is gone.
- `amber: 0.5` with a count means "any". `red: 2.5` means "three or more".

## Group with `by` to save quota

A signal with `{service}` runs once per attaching service. With `by: service_name` and no
`{service}`, vitals runs it once and hands each service the series whose label matches its name or
one of its `otel` names. For defaults that every Cloud Run service gets, this turns dozens of
queries into one.

```yaml
cloud-run-memory:
  promql: 'histogram_quantile(0.99, sum by (service_name, le) (rate(run_googleapis_com:container_memory_utilizations_bucket{monitored_resource="cloud_run_revision"}[{window}])))'
  by: service_name
  amber: 0.85
  red: 0.95
  unit: percent
  noData: ok
```

## Signal ideas

These catch failures that often get past alerting. All are in
[examples/vitals.example.yaml](../examples/vitals.example.yaml).

| Signal | Why |
| --- | --- |
| Cloud Run 5xx ratio | The basic one. Attach it to every service through `defaults`. |
| Cloud Run 429/503 ratio | "No available instance" at max scale. Each one burns a Pub/Sub push attempt or a client retry. |
| Cloud Run p99 memory utilization | Services run out of memory well before anything else pages. |
| Failed liveness probes, worst 2-minute burst | A probe with a tight timeout kills instances that are busy, not broken. Low CPU with failing probes means blocked threads. |
| p95 latency against a day earlier | Regressions after a deploy. Gate it on traffic so a quiet service does not trip it. |
| Messages published against a week earlier | An upstream that stopped producing. Nothing errors, the topic just goes quiet. Use `below: true`. |
| Spanner high-priority CPU | Google recommends staying under 65% on a regional instance. |
| Spanner API error ratio | Deadline exceeded and aborted transactions show here before users see them. |
| Dataflow watermark age and failed jobs | A streaming job that is stuck or dead. |
| Quota exceeded | A Google Cloud quota rejects requests and the caller's retries hide it. |
| Uptime check pass ratio | The outside view. `below: true`. |
| Pub/Sub lag and dead letters | Built in: the `pubsub` source reads every subscription, no signal needed. |

## Templates with parameters

Any `{name}` that is not built in becomes a parameter each attachment must set, so one signal can
serve many topics or instances:

```yaml
signals:
  spanner-high-cpu:
    promql: 'max(spanner_googleapis_com:instance_cpu_utilization_by_priority{instance_id="{instance}",priority="high"})'
    amber: 0.45
    red: 0.65
    unit: percent
systems:
  orders-db:
    signals: [{ use: spanner-high-cpu, instance: acme-orders }]
```
