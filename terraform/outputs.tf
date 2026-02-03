output "lambda_function_name" {
  value = aws_lambda_function.transformer.function_name
}

output "lambda_function_arn" {
  value = aws_lambda_function.transformer.arn
}

output "firehose_delivery_stream_name" {
  value = aws_kinesis_firehose_delivery_stream.metrics.name
}

output "s3_archive_bucket_name" {
  value = aws_s3_bucket.firehose_archive.bucket
}
