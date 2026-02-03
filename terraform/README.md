# Terraform 部署示例

本目录提供 Terraform 示例，用于部署 Lambda 转换函数 + Firehose + S3 归档。

## 打包 Lambda

在项目根目录执行：

```bash
GOOS=linux GOARCH=arm64 go build -o bootstrap main.go
zip -j bootstrap.zip bootstrap
```

## 使用方式

```bash
cd terraform
terraform init
terraform apply \
  -var="lambda_zip_path=../bootstrap.zip" \
  -var="otel_exporter_otlp_endpoint=collector.example.com:4317"
```

### 可选参数

- `vpc_subnet_ids`：私有子网 ID 列表（用于访问内网 OTEL Collector）
- `vpc_security_group_ids`：Lambda 安全组 ID 列表
- `static_labels`：静态标签 JSON 数组
- `firehose_output_mode`：`pass_through` 或 `enhanced`
- `firehose_name`：Firehose 名称
- `s3_bucket_name`：S3 归档桶名（留空自动生成）
- `s3_prefix`：归档前缀
- `s3_error_prefix`：失败记录前缀
- `firehose_buffer_size`：缓冲大小（MB）
- `firehose_buffer_interval`：缓冲时间（秒）

## Firehose 配置

部署后 Terraform 会输出 Firehose 名称与 S3 桶名。
CloudWatch Metric Streams 的目标请设置为该 Firehose。
