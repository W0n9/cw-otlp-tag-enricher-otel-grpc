# CloudWatch OTLP Tag Enricher & OTEL gRPC Forwarder

[中文说明](README.zh-CN.md)

This project is an AWS Lambda (Firehose transformation function) that:

1. Reads **OTLP 1.0.0** (size-delimited protobuf) metrics from CloudWatch Metric Streams (Summary type, KeyValue attributes, Dimensions as Map)
2. Enriches metric labels with AWS resource tags
3. Forwards metrics to your OpenTelemetry Collector via OTLP/gRPC

## Pipeline

CloudWatch Metric Streams → Kinesis Data Firehose → Lambda (this project) → OTEL Collector (gRPC)

The Lambda runs as a Firehose transformation function. By default it **returns the original records**; the enriched metrics are sent to your OTEL Collector only.

## Environment Variables

### OTEL export

- `OTEL_EXPORTER_OTLP_ENDPOINT` (required): OTEL Collector gRPC address, e.g. `collector.example.com:4317`
- `OTEL_EXPORTER_OTLP_INSECURE`: Use plaintext connection, default `true`
- `OTEL_EXPORTER_OTLP_TIMEOUT`: gRPC timeout, default `5s`
- `CONTINUE_ON_EXPORT_FAILURE`: Continue processing when export fails, default `true`

### Firehose output mode

- `FIREHOSE_OUTPUT_MODE`:
  - `pass_through` (default): Return original records
  - `enhanced`: Return enriched OTLP records

### Tag enrichment & cache

- `CONTINUE_ON_RESOURCE_FAILURE`: Continue when resource lookup fails, default `true`
- `FILE_CACHE_ENABLED`: Enable local file cache, default `true`
- `FILE_CACHE_PATH`: Cache directory, default `/tmp`
- `FILE_CACHE_EXPIRATION`: Cache TTL, default `1h`
- `STATIC_LABELS`: Static labels as JSON array, e.g. `["env=prod","team=platform"]`; emitted as `custom_tag_*`, aligned with YACE context custom tags
- `DEFAULT_LABELS`: Also add static labels when resource cannot be matched, default `false`
- `LABELS_SNAKE_CASE`: Convert label keys to snake_case (same as YACE `labelsSnakeCase`), default `true`
- `EXPORTED_TAGS_ON_METRICS`: Optional. JSON array of resource tag keys to export, e.g. `["Name","Environment","Team"]`; if unset or empty, all tags for the resource are exported (same semantics as YACE `exportedTagsOnMetrics`)
- `LOG_LEVEL`: Log level, `debug` or default `info`

### YACE compatibility mode (recommended)

- `YACE_COMPAT_MODE`: Enable YACE compatibility mode, default `false`. Set to `true` to convert CloudWatch Metric Streams Summary metrics into separate Gauge metrics fully compatible with YACE
- `YACE_COMPAT_STATS`: JSON array of statistics to export, default `["Maximum","Minimum","Average","Sum","SampleCount"]`. You can add percentiles, e.g. `["Maximum","Minimum","Average","Sum","SampleCount","p95","p99"]`

## Required IAM permissions

The Lambda execution role needs (for tag enrichment):

- `tag:GetResources`
- `cloudwatch:GetMetricData`
- `cloudwatch:GetMetricStatistics`
- `cloudwatch:ListMetrics`
- Service APIs used by YACE association logic (e.g. `ec2:Describe*`, `apigateway:GET`, etc.)

See the [cloudwatch-metric-streams-lambda-transformation](https://github.com/coralogix/cloudwatch-metric-streams-lambda-transformation) project for a full permission list.

## Local tests

```bash
go test ./...
```

## Terraform deployment

See `terraform/README.md`.

## Notes

This project builds on the tag-enrichment logic of [cloudwatch-metric-streams-lambda-transformation](https://github.com/coralogix/cloudwatch-metric-streams-lambda-transformation) and adds OTLP/gRPC forwarding, so you can send CloudWatch metrics into your own OTEL stack.

**OTLP format**: Only **CloudWatch Metric Streams OTLP 1.0.0 output** is supported (Summary metrics, KeyValue data point attributes, Dimensions as kvlist Map). For 0.7.0 format (DoubleSummary + StringKeyValue), configure the Metric Stream to use 1.0.0.

**YACE compatibility**: Metric names and label layout match [yet-another-cloudwatch-exporter](https://github.com/prometheus-community/yet-another-cloudwatch-exporter) (YACE), so you can query and alert on the same Prometheus/Grafana setup as YACE-sourced metrics:

- **Metric name**: `BuildMetricName(namespace, metricName, statistic)`, e.g. `aws_ec2_cpuutilization_maximum`
- **Labels** (YACE-compatible):

  - `region`: AWS region (from OTLP Resource `cloud.region` or Lambda env `AWS_REGION`)
  - `account_id`: AWS account ID (from OTLP Resource `cloud.account.id`)
  - `namespace`: CloudWatch namespace, e.g. `AWS/EC2`
  - `name`: Resource ARN or `global` when no resource is matched
  - `dimension_*`: CloudWatch dimensions, e.g. `dimension_instance_id`
  - `tag_*`: AWS resource tags, e.g. `tag_name`, `tag_environment`
  - `custom_tag_*`: Static labels from `STATIC_LABELS`

  Label names follow YACE `PromStringTag` rules (snake_case by default).

## YACE compatibility mode in detail

### Background

CloudWatch Metric Streams OTLP 1.0.0 uses **Summary** metrics with a structure like:

```
SummaryDataPoint {
  count: 10          // SampleCount
  sum: 50.0          // Sum
  quantile_values: [
    {quantile: 0.0, value: 2.0}    // Minimum
    {quantile: 0.95, value: 8.0}   // p95
    {quantile: 1.0, value: 10.0}   // Maximum
  ]
}
```

YACE instead emits **one Gauge per statistic**:

- `aws_ec2_cpuutilization_maximum`
- `aws_ec2_cpuutilization_minimum`
- `aws_ec2_cpuutilization_average`
- `aws_ec2_cpuutilization_sum`
- `aws_ec2_cpuutilization_sample_count`

Without YACE compatibility mode, an OpenTelemetry Collector will turn Summaries into Prometheus-style `_count` / `_sum` metrics, not the `_maximum`, `_minimum`, etc. gauges, so YACE Grafana dashboards do not apply as-is.

### Enabling

Set:

```bash
YACE_COMPAT_MODE=true
```

The Lambda then converts Summary metrics into separate Gauges:

| Before (Summary field) | After (Gauge metric name)             |
| ---------------------- | ------------------------------------- |
| `count`                | `aws_ec2_cpuutilization_sample_count` |
| `sum`                  | `aws_ec2_cpuutilization_sum`          |
| `sum / count`          | `aws_ec2_cpuutilization_average`      |
| `quantile: 0.0`        | `aws_ec2_cpuutilization_minimum`      |
| `quantile: 1.0`        | `aws_ec2_cpuutilization_maximum`      |
| `quantile: 0.95`       | `aws_ec2_cpuutilization_p95`          |
| `quantile: 0.99`       | `aws_ec2_cpuutilization_p99`          |

### Custom statistics

Use `YACE_COMPAT_STATS` to choose which statistics to export:

```bash
# Only Maximum and Average
YACE_COMPAT_STATS='["Maximum","Average"]'

# Default stats plus percentiles
YACE_COMPAT_STATS='["Maximum","Minimum","Average","Sum","SampleCount","p95","p99","p99_9"]'
```

Note: Percentiles only have data if the CloudWatch Metric Stream is configured with the corresponding statistics. By default only `Minimum`, `Maximum`, `SampleCount`, and `Sum` are available.
