# CloudWatch OTLP 标签增强与 OTEL gRPC 转发

[English](README.md)

这个项目是一个 AWS Lambda（Firehose 转换函数），用于：

1. 读取 CloudWatch Metric Streams 输出的 **OTLP 1.0.0**（size-delimited protobuf）指标数据（Summary 类型，属性为 KeyValue，Dimensions 为 Map）
2. 按 AWS 资源标签增强指标标签
3. 通过 OTLP/gRPC 转发到自建的 OpenTelemetry Collector

## 工作流程

CloudWatch Metric Streams -> Kinesis Data Firehose -> Lambda（本项目） -> OTEL Collector（gRPC）

Lambda 仍作为 Firehose 转换函数运行，默认**返回原始记录**，仅用于向自建 OTEL 发送增强后的指标。

## 环境变量

### OTEL 发送相关

- `OTEL_EXPORTER_OTLP_ENDPOINT`：必填。OTEL Collector gRPC 地址，例如 `collector.example.com:4317`
- `OTEL_EXPORTER_OTLP_INSECURE`：是否使用明文连接，默认 `true`
- `OTEL_EXPORTER_OTLP_TIMEOUT`：gRPC 超时，默认 `5s`
- `CONTINUE_ON_EXPORT_FAILURE`：发送失败是否继续处理，默认 `true`

### Firehose 输出模式

- `FIREHOSE_OUTPUT_MODE`：
  - `pass_through`（默认）：返回原始记录
  - `enhanced`：返回增强后的 OTLP 记录

### 标签增强与缓存

- `CONTINUE_ON_RESOURCE_FAILURE`：资源查询失败时是否继续，默认 `true`
- `FILE_CACHE_ENABLED`：是否启用本地缓存，默认 `true`
- `FILE_CACHE_PATH`：缓存目录，默认 `/tmp`
- `FILE_CACHE_EXPIRATION`：缓存有效期，默认 `1h`
- `STATIC_LABELS`：静态标签，JSON 数组，如 `["env=prod","team=platform"]`；输出为 `custom_tag_*`，与 YACE 的 context custom tags 一致
- `DEFAULT_LABELS`：当资源无法匹配时，也添加静态标签，默认 `false`
- `LABELS_SNAKE_CASE`：标签 key 是否转为 snake_case（与 YACE 的 `labelsSnakeCase` 一致），默认 `true`
- `EXPORTED_TAGS_ON_METRICS`：可选。要导出的资源 tag key 列表，JSON 数组，如 `["Name","Environment","Team"]`；未设置或为空时导出该资源全部 tag，与 YACE 的 `exportedTagsOnMetrics` 语义一致
- `LOG_LEVEL`：日志级别，`debug` 或默认 `info`

### YACE 兼容模式（推荐）

- `YACE_COMPAT_MODE`：是否启用 YACE 兼容模式，默认 `false`。设为 `true` 可将 CloudWatch Metric Streams 的 Summary 指标转换为与 YACE 完全兼容的多个独立 Gauge 指标
- `YACE_COMPAT_STATS`：要导出的统计类型列表，JSON 数组，默认 `["Maximum","Minimum","Average","Sum","SampleCount"]`。可根据需要添加百分位数如 `["Maximum","Minimum","Average","Sum","SampleCount","p95","p99"]`

## 必要权限

Lambda 执行角色需要以下权限（与标签增强相关）：

- `tag:GetResources`
- `cloudwatch:GetMetricData`
- `cloudwatch:GetMetricStatistics`
- `cloudwatch:ListMetrics`
- 以及与 YACE 关联逻辑相关的服务 API（例如 `ec2:Describe*`、`apigateway:GET` 等）

可参考 [cloudwatch-metric-streams-lambda-transformation](https://github.com/coralogix/cloudwatch-metric-streams-lambda-transformation) 项目的权限列表。

## 本地测试

```bash
go test ./...
```

## Terraform 部署示例

见 `terraform/README.md`。

## 说明

本项目基于 [cloudwatch-metric-streams-lambda-transformation](https://github.com/coralogix/cloudwatch-metric-streams-lambda-transformation) 的标签增强逻辑，
并增加了 OTLP/gRPC 转发能力，适合将 CloudWatch 指标直接接入自建 OTEL 基础设施。

**OTLP 格式**：仅支持 **CloudWatch Metric Streams 的 OTLP 1.0.0 输出**（指标类型为 Summary，数据点属性为 KeyValue，Dimensions 为 kvlist Map）。若使用 0.7.0 格式（DoubleSummary + StringKeyValue），请在 Metric Stream 中配置为 1.0.0 格式。

**YACE 兼容**：指标命名与标签格式与 [yet-another-cloudwatch-exporter](https://github.com/prometheus-community/yet-another-cloudwatch-exporter)（YACE）一致，便于与 YACE 拉取的指标在同一 Prometheus/Grafana 下统一查询与告警：

- **指标名**：`BuildMetricName(namespace, metricName, statistic)`，例如 `aws_ec2_cpuutilization_maximum`
- **标签**（与 YACE 完全兼容）：

  - `region`：AWS 区域（从 OTLP Resource 的 `cloud.region` 属性提取，或使用 Lambda 环境变量 `AWS_REGION`）
  - `account_id`：AWS 账户 ID（从 OTLP Resource 的 `cloud.account.id` 属性提取）
  - `namespace`：CloudWatch 命名空间，如 `AWS/EC2`
  - `name`：资源 ARN 或 `global`（当无法匹配资源时）
  - `dimension_*`：CloudWatch Dimensions，如 `dimension_instance_id`
  - `tag_*`：AWS 资源标签，如 `tag_name`、`tag_environment`
  - `custom_tag_*`：静态标签（来自 `STATIC_LABELS` 环境变量）

  所有标签名均使用 YACE 的 `PromStringTag` 规则（默认转换为 snake_case）

## YACE 兼容模式详解

### 问题背景

CloudWatch Metric Streams 输出的 OTLP 1.0.0 格式使用 **Summary** 类型指标，数据结构如下：

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

而 YACE 为每个统计类型生成**独立的 Gauge 指标**：

- `aws_ec2_cpuutilization_maximum`
- `aws_ec2_cpuutilization_minimum`
- `aws_ec2_cpuutilization_average`
- `aws_ec2_cpuutilization_sum`
- `aws_ec2_cpuutilization_sample_count`

如果不启用 YACE 兼容模式，OpenTelemetry Collector 会将 Summary 转换为 Prometheus 格式的 `_count`、`_sum` 后缀指标，而非 `_maximum`、`_minimum` 等独立指标，导致无法直接复用 YACE 的 Grafana 仪表板。

### 启用方式

设置环境变量：

```bash
YACE_COMPAT_MODE=true
```

启用后，Lambda 会将 CloudWatch Metric Streams 的 Summary 指标转换为多个独立的 Gauge 指标：

| 转换前（Summary 字段） | 转换后（Gauge 指标名）                |
| ---------------------- | ------------------------------------- |
| `count`                | `aws_ec2_cpuutilization_sample_count` |
| `sum`                  | `aws_ec2_cpuutilization_sum`          |
| `sum / count`          | `aws_ec2_cpuutilization_average`      |
| `quantile: 0.0`        | `aws_ec2_cpuutilization_minimum`      |
| `quantile: 1.0`        | `aws_ec2_cpuutilization_maximum`      |
| `quantile: 0.95`       | `aws_ec2_cpuutilization_p95`          |
| `quantile: 0.99`       | `aws_ec2_cpuutilization_p99`          |

### 自定义统计类型

通过 `YACE_COMPAT_STATS` 可指定要导出的统计类型：

```bash
# 只导出 Maximum 和 Average
YACE_COMPAT_STATS='["Maximum","Average"]'

# 导出所有默认统计类型 + 百分位数
YACE_COMPAT_STATS='["Maximum","Minimum","Average","Sum","SampleCount","p95","p99","p99_9"]'
```

注意：百分位数需要在 CloudWatch Metric Stream 中配置额外的统计类型才会有数据。默认只有 `Minimum`、`Maximum`、`SampleCount`、`Sum`。
