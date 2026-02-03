terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {}

data "aws_iam_policy_document" "assume_role" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "lambda_role" {
  name               = "${var.lambda_name}-role"
  assume_role_policy = data.aws_iam_policy_document.assume_role.json
}

resource "aws_iam_role_policy_attachment" "basic" {
  role       = aws_iam_role.lambda_role.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

data "aws_iam_policy_document" "lambda_policy" {
  statement {
    actions = [
      "tag:GetResources",
      "cloudwatch:GetMetricData",
      "cloudwatch:GetMetricStatistics",
      "cloudwatch:ListMetrics",
      "apigateway:GET",
      "aps:ListWorkspaces",
      "autoscaling:DescribeAutoScalingGroups",
      "dms:DescribeReplicationInstances",
      "dms:DescribeReplicationTasks",
      "ec2:DescribeTransitGatewayAttachments",
      "ec2:DescribeSpotFleetRequests",
      "shield:ListProtections",
      "storagegateway:ListGateways",
      "storagegateway:ListTagsForResource",
      "iam:ListAccountAliases"
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "lambda_inline" {
  name   = "${var.lambda_name}-policy"
  role   = aws_iam_role.lambda_role.id
  policy = data.aws_iam_policy_document.lambda_policy.json
}

resource "aws_cloudwatch_log_group" "lambda" {
  name              = "/aws/lambda/${var.lambda_name}"
  retention_in_days = 14
}

locals {
  use_vpc        = length(var.vpc_subnet_ids) > 0 && length(var.vpc_security_group_ids) > 0
  s3_bucket_name = var.s3_bucket_name != "" ? var.s3_bucket_name : "${var.firehose_name}-${data.aws_caller_identity.current.account_id}-${data.aws_region.current.name}"
}

data "aws_caller_identity" "current" {}

data "aws_region" "current" {}

resource "aws_lambda_function" "transformer" {
  function_name    = var.lambda_name
  role             = aws_iam_role.lambda_role.arn
  handler          = "bootstrap"
  runtime          = "provided.al2"
  architectures    = [var.lambda_architecture]
  filename         = var.lambda_zip_path
  source_code_hash = filebase64sha256(var.lambda_zip_path)
  memory_size      = var.lambda_memory_size
  timeout          = var.lambda_timeout

  environment {
    variables = {
      OTEL_EXPORTER_OTLP_ENDPOINT  = var.otel_exporter_otlp_endpoint
      OTEL_EXPORTER_OTLP_INSECURE  = tostring(var.otel_exporter_otlp_insecure)
      OTEL_EXPORTER_OTLP_TIMEOUT   = var.otel_exporter_otlp_timeout
      CONTINUE_ON_EXPORT_FAILURE   = tostring(var.continue_on_export_failure)
      CONTINUE_ON_RESOURCE_FAILURE = tostring(var.continue_on_resource_failure)
      FILE_CACHE_ENABLED           = tostring(var.file_cache_enabled)
      FILE_CACHE_PATH              = var.file_cache_path
      FILE_CACHE_EXPIRATION        = var.file_cache_expiration
      STATIC_LABELS                = var.static_labels
      DEFAULT_LABELS               = tostring(var.default_labels)
      FIREHOSE_OUTPUT_MODE         = var.firehose_output_mode
      LOG_LEVEL                    = var.log_level
    }
  }

  dynamic "vpc_config" {
    for_each = local.use_vpc ? [1] : []
    content {
      subnet_ids         = var.vpc_subnet_ids
      security_group_ids = var.vpc_security_group_ids
    }
  }

  depends_on = [
    aws_iam_role_policy_attachment.basic,
    aws_iam_role_policy.lambda_inline,
    aws_cloudwatch_log_group.lambda
  ]
}

resource "aws_s3_bucket" "firehose_archive" {
  bucket = local.s3_bucket_name
}

resource "aws_s3_bucket_lifecycle_configuration" "firehose_archive" {
  bucket = aws_s3_bucket.firehose_archive.id

  rule {
    id     = "expire-objects-30d"
    status = "Enabled"

    expiration {
      days = 30
    }
  }
}

data "aws_iam_policy_document" "firehose_assume_role" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["firehose.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "firehose_role" {
  name               = "${var.firehose_name}-role"
  assume_role_policy = data.aws_iam_policy_document.firehose_assume_role.json
}

data "aws_iam_policy_document" "firehose_policy" {
  statement {
    actions = [
      "s3:AbortMultipartUpload",
      "s3:GetBucketLocation",
      "s3:GetObject",
      "s3:ListBucket",
      "s3:ListBucketMultipartUploads",
      "s3:PutObject"
    ]
    resources = [
      aws_s3_bucket.firehose_archive.arn,
      "${aws_s3_bucket.firehose_archive.arn}/*"
    ]
  }

  statement {
    actions = [
      "lambda:InvokeFunction",
      "lambda:GetFunctionConfiguration"
    ]
    resources = [
      aws_lambda_function.transformer.arn,
      "${aws_lambda_function.transformer.arn}:*"
    ]
  }
}

resource "aws_iam_role_policy" "firehose_inline" {
  name   = "${var.firehose_name}-policy"
  role   = aws_iam_role.firehose_role.id
  policy = data.aws_iam_policy_document.firehose_policy.json
}

resource "aws_kinesis_firehose_delivery_stream" "metrics" {
  name        = var.firehose_name
  destination = "extended_s3"

  extended_s3_configuration {
    role_arn            = aws_iam_role.firehose_role.arn
    bucket_arn          = aws_s3_bucket.firehose_archive.arn
    prefix              = var.s3_prefix
    error_output_prefix = var.s3_error_prefix
    compression_format  = "GZIP"

    processing_configuration {
      enabled = true

      processors {
        type = "Lambda"

        parameters {
          parameter_name  = "LambdaArn"
          parameter_value = aws_lambda_function.transformer.arn
        }
      }
    }
  }
}
