variable "lambda_name" {
  type        = string
  description = "Lambda function name"
  default     = "cw-otlp-tag-enricher"
}

variable "lambda_zip_path" {
  type        = string
  description = "Path to bootstrap.zip"
}

variable "lambda_architecture" {
  type        = string
  description = "Lambda architecture: arm64 or x86_64"
  default     = "arm64"
}

variable "lambda_memory_size" {
  type        = number
  description = "Lambda memory size in MB"
  default     = 128
}

variable "lambda_timeout" {
  type        = number
  description = "Lambda timeout in seconds"
  default     = 30
}

variable "otel_exporter_otlp_endpoint" {
  type        = string
  description = "OTEL collector gRPC endpoint, e.g. collector.example.com:4317"
}

variable "otel_exporter_otlp_insecure" {
  type        = bool
  description = "Use insecure gRPC connection"
  default     = true
}

variable "otel_exporter_otlp_timeout" {
  type        = string
  description = "OTLP gRPC timeout (Go duration)"
  default     = "5s"
}

variable "continue_on_export_failure" {
  type        = bool
  description = "Continue when OTLP export fails"
  default     = true
}

variable "continue_on_resource_failure" {
  type        = bool
  description = "Continue when resource lookup fails"
  default     = true
}

variable "file_cache_enabled" {
  type        = bool
  description = "Enable local file cache"
  default     = true
}

variable "file_cache_path" {
  type        = string
  description = "Local cache directory"
  default     = "/tmp"
}

variable "file_cache_expiration" {
  type        = string
  description = "Cache expiration (Go duration)"
  default     = "1h"
}

variable "static_labels" {
  type        = string
  description = "Static labels JSON array, e.g. [\"env=prod\"]"
  default     = ""
}

variable "default_labels" {
  type        = bool
  description = "Add static labels even when resource tags missing"
  default     = false
}

variable "firehose_output_mode" {
  type        = string
  description = "pass_through or enhanced"
  default     = "pass_through"
}

variable "log_level" {
  type        = string
  description = "Log level: info or debug"
  default     = "info"
}

variable "vpc_subnet_ids" {
  type        = list(string)
  description = "Subnets for Lambda VPC config"
  default     = []
}

variable "vpc_security_group_ids" {
  type        = list(string)
  description = "Security groups for Lambda VPC config"
  default     = []
}

variable "firehose_name" {
  type        = string
  description = "Kinesis Firehose delivery stream name"
  default     = "cw-metric-stream-firehose"
}

variable "s3_bucket_name" {
  type        = string
  description = "S3 bucket name for Firehose archive (leave empty to auto-generate)"
  default     = ""
}

variable "s3_prefix" {
  type        = string
  description = "S3 prefix for delivered objects"
  default     = "cloudwatch-metrics/"
}

variable "s3_error_prefix" {
  type        = string
  description = "S3 prefix for failed delivery"
  default     = "cloudwatch-metrics-errors/"
}

variable "firehose_buffer_size" {
  type        = number
  description = "Firehose buffer size in MB"
  default     = 1
}

variable "firehose_buffer_interval" {
  type        = number
  description = "Firehose buffer interval in seconds"
  default     = 60
}
