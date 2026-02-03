package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/prometheus-community/yet-another-cloudwatch-exporter/pkg/config"
	"github.com/prometheus-community/yet-another-cloudwatch-exporter/pkg/job/maxdimassociator"
	"github.com/prometheus-community/yet-another-cloudwatch-exporter/pkg/model"
	"github.com/prometheus-community/yet-another-cloudwatch-exporter/pkg/promutil"
	metricsservicepb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func TestParseStaticLabels(t *testing.T) {
	labels, err := parseStaticLabels(`["env=prod","team=platform"]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if labels["env"] != "prod" {
		t.Fatalf("expected env=prod, got %q", labels["env"])
	}
	if labels["team"] != "platform" {
		t.Fatalf("expected team=platform, got %q", labels["team"])
	}
}

func TestParseExportedTags(t *testing.T) {
	tags, err := parseExportedTags(`["Name","Environment","Team"]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tags) != 3 || tags[0] != "Name" || tags[1] != "Environment" || tags[2] != "Team" {
		t.Fatalf("expected [Name Environment Team], got %v", tags)
	}
	empty, err := parseExportedTags("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if empty != nil {
		t.Fatalf("expected nil for empty env, got %v", empty)
	}
}

func TestRequestsRoundTrip(t *testing.T) {
	req := makeExportRequestOTLP10("amazonaws.com/AWS/EC2/CPUUtilization", []*commonpb.KeyValue{
		{Key: "Namespace", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "AWS/EC2"}}},
		{Key: "MetricName", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "CPUUtilization"}}},
		{Key: "Dimensions", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{
			Values: []*commonpb.KeyValue{
				{Key: "InstanceId", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "i-1234567890"}}},
			},
		}}}},
	})
	raw, err := requestsIntoRawData([]*metricsservicepb.ExportMetricsServiceRequest{req})
	if err != nil {
		t.Fatalf("requestsIntoRawData failed: %v", err)
	}
	out, err := rawDataIntoRequests(raw)
	if err != nil {
		t.Fatalf("rawDataIntoRequests failed: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 request, got %d", len(out))
	}
	gotName := out[0].GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics()[0].GetName()
	if gotName != "amazonaws.com/AWS/EC2/CPUUtilization" {
		t.Fatalf("metric name mismatch after round-trip: got %q", gotName)
	}
}

// makeExportRequestOTLP10 builds an OTLP 1.0 ExportMetricsServiceRequest with one Summary data point and the given attributes.
func makeExportRequestOTLP10(metricName string, attrs []*commonpb.KeyValue) *metricsservicepb.ExportMetricsServiceRequest {
	return makeExportRequestOTLP10WithResource(metricName, attrs, "", "")
}

// makeExportRequestOTLP10WithResource builds an OTLP 1.0 ExportMetricsServiceRequest with resource attributes for account_id and region.
func makeExportRequestOTLP10WithResource(metricName string, attrs []*commonpb.KeyValue, accountID, region string) *metricsservicepb.ExportMetricsServiceRequest {
	var resourceAttrs []*commonpb.KeyValue
	if accountID != "" {
		resourceAttrs = append(resourceAttrs, &commonpb.KeyValue{
			Key:   "cloud.account.id",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: accountID}},
		})
	}
	if region != "" {
		resourceAttrs = append(resourceAttrs, &commonpb.KeyValue{
			Key:   "cloud.region",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: region}},
		})
	}

	rm := &metricspb.ResourceMetrics{
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Metrics: []*metricspb.Metric{{
				Name: metricName,
				Data: &metricspb.Metric_Summary{
					Summary: &metricspb.Summary{
						DataPoints: []*metricspb.SummaryDataPoint{{
							Attributes: attrs,
						}},
					},
				},
			}},
		}},
	}
	if len(resourceAttrs) > 0 {
		rm.Resource = &resourcepb.Resource{Attributes: resourceAttrs}
	}

	return &metricsservicepb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{rm},
	}
}

// ec2InputAttrsOTLP10 returns OTLP 1.0 attributes for AWS/EC2 CPUUtilization with InstanceId dimension.
func ec2InputAttrsOTLP10(instanceID string) []*commonpb.KeyValue {
	return []*commonpb.KeyValue{
		{Key: "Namespace", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "AWS/EC2"}}},
		{Key: "MetricName", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "CPUUtilization"}}},
		{Key: "Dimensions", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{
			Values: []*commonpb.KeyValue{
				{Key: "InstanceId", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: instanceID}}},
			},
		}}}},
	}
}

func TestBuildCloudWatchMetricFromKeyValues(t *testing.T) {
	attrs := []*commonpb.KeyValue{
		{Key: "MetricName", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "VolumeWriteBytes"}}},
		{Key: "Namespace", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "AWS/EBS"}}},
		{Key: "Dimensions", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{
			Values: []*commonpb.KeyValue{
				{Key: "VolumeId", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "vol-123"}}},
			},
		}}}},
	}
	cwm := buildCloudWatchMetricFromKeyValues(attrs)
	if cwm.MetricName != "VolumeWriteBytes" {
		t.Fatalf("expected MetricName VolumeWriteBytes, got %q", cwm.MetricName)
	}
	if cwm.Namespace != "AWS/EBS" {
		t.Fatalf("expected Namespace AWS/EBS, got %q", cwm.Namespace)
	}
	if len(cwm.Dimensions) != 1 || cwm.Dimensions[0].Name != "VolumeId" {
		t.Fatalf("expected VolumeId dimension, got %+v", cwm.Dimensions)
	}
}

// mockTaggingClient is used when resourceCache is pre-filled so GetResources is never called.
type mockTaggingClient struct{}

func (mockTaggingClient) GetResources(ctx context.Context, job model.DiscoveryJob, region string) ([]*model.TaggedResource, error) {
	return nil, errors.New("mock: should not be called")
}

// keyValueToMap converts OTLP 1.0 KeyValue attributes (string values only) to a map for assertions.
func keyValueToMap(attrs []*commonpb.KeyValue) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		if a != nil && a.GetValue() != nil {
			m[a.GetKey()] = a.GetValue().GetStringValue()
		}
	}
	return m
}

// TestEnhanceEC2CPUUtilization verifies that for input:
//   - Namespace: AWS/EC2, MetricName: CPUUtilization, Dimensions: InstanceId=i-1234567890abcdef0
//   - OTLP 1.0 attributes (Namespace, MetricName, Dimensions kvlist)
//
// the enricher produces YACE-compatible output: metric name from BuildMetricName, attributes region/account_id/namespace/name/dimension_*/tag_*.
func TestEnhanceEC2CPUUtilization(t *testing.T) {
	ec2ARN := "arn:aws:ec2:us-east-1:123456789012:instance/i-1234567890abcdef0"
	ec2Resource := &model.TaggedResource{
		ARN:       ec2ARN,
		Namespace: "AWS/EC2",
		Region:    "us-east-1",
		Tags: []model.Tag{
			{Key: "Name", Value: "my-instance"},
			{Key: "Environment", Value: "prod"},
		},
	}
	// Use the new helper with resource attributes for account_id and region
	req := makeExportRequestOTLP10WithResource(
		"amazonaws.com/AWS/EC2/CPUUtilization",
		ec2InputAttrsOTLP10("i-1234567890abcdef0"),
		"123456789012", // account_id
		"us-east-1",    // region
	)

	logger := slog.Default()
	resourceCache := map[string][]*model.TaggedResource{"AWS/EC2": {ec2Resource}}
	svc := config.SupportedServices.GetService("AWS/EC2")
	if svc == nil {
		t.Fatal("AWS/EC2 service not found in config")
	}
	associatorCache := map[string]maxdimassociator.Associator{
		"AWS/EC2": maxdimassociator.NewAssociator(logger, svc.ToModelDimensionsRegexp(), resourceCache["AWS/EC2"]),
	}

	err := enhanceRequests(
		logger, "/tmp", true,
		[]*metricsservicepb.ExportMetricsServiceRequest{req},
		resourceCache, associatorCache,
		aws.String("us-east-1"), mockTaggingClient{},
		0, false, nil, false,
		true, nil,
		false, nil, // yaceCompatMode=false
	)
	if err != nil {
		t.Fatalf("enhanceRequests failed: %v", err)
	}

	metric := req.GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics()[0]
	dp := metric.GetSummary().GetDataPoints()[0]
	got := keyValueToMap(dp.GetAttributes())

	expectedName := promutil.BuildMetricName("AWS/EC2", "CPUUtilization", "")
	if metric.GetName() != expectedName {
		t.Errorf("metric.Name: got %q, want %q", metric.GetName(), expectedName)
	}

	// Verify new YACE context labels
	if got["region"] != "us-east-1" {
		t.Errorf("region: got %q, want %q", got["region"], "us-east-1")
	}
	if got["account_id"] != "123456789012" {
		t.Errorf("account_id: got %q, want %q", got["account_id"], "123456789012")
	}
	if got["namespace"] != "AWS/EC2" {
		t.Errorf("namespace: got %q, want %q", got["namespace"], "AWS/EC2")
	}
	if got["name"] != ec2ARN {
		t.Errorf("name: got %q, want %q", got["name"], ec2ARN)
	}
	if got["dimension_instance_id"] != "i-1234567890abcdef0" {
		t.Errorf("dimension_instance_id: got %q", got["dimension_instance_id"])
	}
	if got["tag_name"] != "my-instance" {
		t.Errorf("tag_name: got %q", got["tag_name"])
	}
	if got["tag_environment"] != "prod" {
		t.Errorf("tag_environment: got %q", got["tag_environment"])
	}
	// Expected labels: region, account_id, namespace, name, dimension_instance_id, tag_name, tag_environment = 7
	if len(got) != 7 {
		t.Errorf("expected exactly 7 YACE attributes, got %d: %v", len(got), got)
	}
}

// TestEnhanceEC2WithStaticLabelsAndExportedTags verifies custom_tag_* from STATIC_LABELS and EXPORTED_TAGS_ON_METRICS (only exported tags appear as tag_*).
func TestEnhanceEC2WithStaticLabelsAndExportedTags(t *testing.T) {
	ec2Resource := &model.TaggedResource{
		ARN:       "arn:aws:ec2:us-east-1:123456789012:instance/i-1234567890abcdef0",
		Namespace: "AWS/EC2",
		Region:    "us-east-1",
		Tags: []model.Tag{
			{Key: "Name", Value: "my-instance"},
			{Key: "Environment", Value: "prod"},
		},
	}
	req := makeExportRequestOTLP10WithResource("ignored", ec2InputAttrsOTLP10("i-1234567890abcdef0"), "123456789012", "us-east-1")
	logger := slog.Default()
	resourceCache := map[string][]*model.TaggedResource{"AWS/EC2": {ec2Resource}}
	svc := config.SupportedServices.GetService("AWS/EC2")
	if svc == nil {
		t.Fatal("AWS/EC2 service not found")
	}
	associatorCache := map[string]maxdimassociator.Associator{
		"AWS/EC2": maxdimassociator.NewAssociator(logger, svc.ToModelDimensionsRegexp(), resourceCache["AWS/EC2"]),
	}
	staticLabels := map[string]string{"env": "prod"}
	exportedTags := []string{"Name"}

	err := enhanceRequests(
		logger, "/tmp", true,
		[]*metricsservicepb.ExportMetricsServiceRequest{req},
		resourceCache, associatorCache,
		aws.String("us-east-1"), mockTaggingClient{},
		0, false, staticLabels, false,
		true, exportedTags,
		false, nil, // yaceCompatMode=false
	)
	if err != nil {
		t.Fatalf("enhanceRequests failed: %v", err)
	}

	got := keyValueToMap(req.GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics()[0].GetSummary().GetDataPoints()[0].GetAttributes())
	if got["tag_name"] != "my-instance" {
		t.Errorf("tag_name: got %q", got["tag_name"])
	}
	if _, ok := got["tag_environment"]; ok {
		t.Errorf("tag_environment should be absent when EXPORTED_TAGS_ON_METRICS=[\"Name\"]")
	}
	if got["custom_tag_env"] != "prod" {
		t.Errorf("custom_tag_env: got %q", got["custom_tag_env"])
	}
	// Verify new labels are present
	if got["region"] != "us-east-1" {
		t.Errorf("region: got %q", got["region"])
	}
	if got["account_id"] != "123456789012" {
		t.Errorf("account_id: got %q", got["account_id"])
	}
}

func TestExtractResourceAttributes(t *testing.T) {
	// Test with both account_id and region
	rm := &metricspb.ResourceMetrics{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				{Key: "cloud.account.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "123456789012"}}},
				{Key: "cloud.region", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "us-west-2"}}},
				{Key: "cloud.provider", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "aws"}}},
			},
		},
	}
	accountID, region := extractResourceAttributes(rm)
	if accountID != "123456789012" {
		t.Errorf("accountID: got %q, want %q", accountID, "123456789012")
	}
	if region != "us-west-2" {
		t.Errorf("region: got %q, want %q", region, "us-west-2")
	}

	// Test with nil resource
	rm2 := &metricspb.ResourceMetrics{}
	accountID2, region2 := extractResourceAttributes(rm2)
	if accountID2 != "" || region2 != "" {
		t.Errorf("expected empty values for nil resource, got accountID=%q, region=%q", accountID2, region2)
	}

	// Test with nil ResourceMetrics
	accountID3, region3 := extractResourceAttributes(nil)
	if accountID3 != "" || region3 != "" {
		t.Errorf("expected empty values for nil ResourceMetrics, got accountID=%q, region=%q", accountID3, region3)
	}
}

func TestQuantileToStatistic(t *testing.T) {
	tests := []struct {
		quantile float64
		expected string
	}{
		{0.0, "Minimum"},
		{1.0, "Maximum"},
		{0.5, "p50"},
		{0.95, "p95"},
		{0.99, "p99"},
		{0.999, "p99_9"},
	}
	for _, tc := range tests {
		got := quantileToStatistic(tc.quantile)
		if got != tc.expected {
			t.Errorf("quantileToStatistic(%v): got %q, want %q", tc.quantile, got, tc.expected)
		}
	}
}

func TestParseYACEStats(t *testing.T) {
	// Test default (empty string)
	stats, err := parseYACEStats("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stats["Maximum"] || !stats["Minimum"] || !stats["Average"] || !stats["Sum"] || !stats["SampleCount"] {
		t.Errorf("expected all default stats to be enabled, got %v", stats)
	}

	// Test custom list
	stats, err = parseYACEStats(`["Maximum","Average"]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stats["Maximum"] || !stats["Average"] {
		t.Errorf("expected Maximum and Average to be enabled")
	}
	if stats["Minimum"] || stats["Sum"] || stats["SampleCount"] {
		t.Errorf("expected Minimum, Sum, SampleCount to be disabled")
	}

	// Test invalid JSON
	_, err = parseYACEStats(`invalid`)
	if err == nil {
		t.Errorf("expected error for invalid JSON")
	}
}

// makeExportRequestWithSummaryData builds an OTLP 1.0 ExportMetricsServiceRequest with Summary data including count, sum, and quantiles.
func makeExportRequestWithSummaryData(metricName string, attrs []*commonpb.KeyValue, count uint64, sum float64, quantiles map[float64]float64) *metricsservicepb.ExportMetricsServiceRequest {
	return makeExportRequestWithSummaryDataAndResource(metricName, attrs, count, sum, quantiles, "", "")
}

// makeExportRequestWithSummaryDataAndResource builds an OTLP 1.0 ExportMetricsServiceRequest with Summary data and resource attributes.
func makeExportRequestWithSummaryDataAndResource(metricName string, attrs []*commonpb.KeyValue, count uint64, sum float64, quantiles map[float64]float64, accountID, region string) *metricsservicepb.ExportMetricsServiceRequest {
	var qvs []*metricspb.SummaryDataPoint_ValueAtQuantile
	for q, v := range quantiles {
		qvs = append(qvs, &metricspb.SummaryDataPoint_ValueAtQuantile{
			Quantile: q,
			Value:    v,
		})
	}

	var resourceAttrs []*commonpb.KeyValue
	if accountID != "" {
		resourceAttrs = append(resourceAttrs, &commonpb.KeyValue{
			Key:   "cloud.account.id",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: accountID}},
		})
	}
	if region != "" {
		resourceAttrs = append(resourceAttrs, &commonpb.KeyValue{
			Key:   "cloud.region",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: region}},
		})
	}

	rm := &metricspb.ResourceMetrics{
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Metrics: []*metricspb.Metric{{
				Name: metricName,
				Data: &metricspb.Metric_Summary{
					Summary: &metricspb.Summary{
						DataPoints: []*metricspb.SummaryDataPoint{{
							Attributes:        attrs,
							Count:             count,
							Sum:               sum,
							QuantileValues:    qvs,
							TimeUnixNano:      1000000000,
							StartTimeUnixNano: 900000000,
						}},
					},
				},
			}},
		}},
	}
	if len(resourceAttrs) > 0 {
		rm.Resource = &resourcepb.Resource{Attributes: resourceAttrs}
	}

	return &metricsservicepb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{rm},
	}
}

func TestSummaryToGauges(t *testing.T) {
	cwm := &model.Metric{
		Namespace:  "AWS/EC2",
		MetricName: "CPUUtilization",
	}
	attrs := []*commonpb.KeyValue{
		{Key: "name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "test-arn"}}},
	}
	dp := &metricspb.SummaryDataPoint{
		Count:             10,
		Sum:               50.0,
		TimeUnixNano:      1000000000,
		StartTimeUnixNano: 900000000,
		QuantileValues: []*metricspb.SummaryDataPoint_ValueAtQuantile{
			{Quantile: 0.0, Value: 2.0},  // Minimum
			{Quantile: 0.95, Value: 8.0}, // p95
			{Quantile: 1.0, Value: 10.0}, // Maximum
		},
	}

	// Enable all stats
	enabledStats := map[string]bool{
		"Maximum": true, "Minimum": true, "Average": true, "Sum": true, "SampleCount": true, "p95": true,
	}

	gauges := summaryToGauges(cwm, dp, attrs, enabledStats)

	// Should produce: SampleCount, Sum, Average, Minimum, p95, Maximum
	expectedNames := map[string]float64{
		"aws_ec2_cpuutilization_sample_count": 10.0,
		"aws_ec2_cpuutilization_sum":          50.0,
		"aws_ec2_cpuutilization_average":      5.0, // 50/10
		"aws_ec2_cpuutilization_minimum":      2.0,
		"aws_ec2_cpuutilization_p95":          8.0,
		"aws_ec2_cpuutilization_maximum":      10.0,
	}

	if len(gauges) != len(expectedNames) {
		t.Fatalf("expected %d gauges, got %d", len(expectedNames), len(gauges))
	}

	for _, g := range gauges {
		expectedVal, ok := expectedNames[g.GetName()]
		if !ok {
			t.Errorf("unexpected gauge name: %s", g.GetName())
			continue
		}
		gotVal := g.GetGauge().GetDataPoints()[0].GetAsDouble()
		if gotVal != expectedVal {
			t.Errorf("gauge %s: got value %v, want %v", g.GetName(), gotVal, expectedVal)
		}
		// Check attributes are preserved
		gotAttrs := keyValueToMap(g.GetGauge().GetDataPoints()[0].GetAttributes())
		if gotAttrs["name"] != "test-arn" {
			t.Errorf("gauge %s: expected name=test-arn in attrs, got %v", g.GetName(), gotAttrs)
		}
	}
}

// TestEnhanceEC2YACECompatMode verifies that with YACE_COMPAT_MODE=true, Summary metrics are converted to multiple Gauge metrics.
func TestEnhanceEC2YACECompatMode(t *testing.T) {
	ec2ARN := "arn:aws:ec2:us-east-1:123456789012:instance/i-1234567890abcdef0"
	ec2Resource := &model.TaggedResource{
		ARN:       ec2ARN,
		Namespace: "AWS/EC2",
		Region:    "us-east-1",
		Tags: []model.Tag{
			{Key: "Name", Value: "my-instance"},
		},
	}

	// Create request with Summary data including count, sum, quantiles, and resource attributes
	req := makeExportRequestWithSummaryDataAndResource(
		"amazonaws.com/AWS/EC2/CPUUtilization",
		ec2InputAttrsOTLP10("i-1234567890abcdef0"),
		10,   // count
		50.0, // sum
		map[float64]float64{
			0.0: 2.0,  // Minimum
			1.0: 10.0, // Maximum
		},
		"123456789012", // account_id
		"us-east-1",    // region
	)

	logger := slog.Default()
	resourceCache := map[string][]*model.TaggedResource{"AWS/EC2": {ec2Resource}}
	svc := config.SupportedServices.GetService("AWS/EC2")
	if svc == nil {
		t.Fatal("AWS/EC2 service not found in config")
	}
	associatorCache := map[string]maxdimassociator.Associator{
		"AWS/EC2": maxdimassociator.NewAssociator(logger, svc.ToModelDimensionsRegexp(), resourceCache["AWS/EC2"]),
	}

	// Enable YACE compat mode with all default stats
	yaceCompatStats, _ := parseYACEStats("")

	err := enhanceRequests(
		logger, "/tmp", true,
		[]*metricsservicepb.ExportMetricsServiceRequest{req},
		resourceCache, associatorCache,
		aws.String("us-east-1"), mockTaggingClient{},
		0, false, nil, false,
		true, nil,
		true, yaceCompatStats, // yaceCompatMode=true
	)
	if err != nil {
		t.Fatalf("enhanceRequests failed: %v", err)
	}

	// Verify metrics were converted from Summary to Gauges
	metrics := req.GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics()

	// Should have: SampleCount, Sum, Average, Minimum, Maximum
	expectedNames := []string{
		"aws_ec2_cpuutilization_sample_count",
		"aws_ec2_cpuutilization_sum",
		"aws_ec2_cpuutilization_average",
		"aws_ec2_cpuutilization_minimum",
		"aws_ec2_cpuutilization_maximum",
	}

	if len(metrics) != len(expectedNames) {
		var gotNames []string
		for _, m := range metrics {
			gotNames = append(gotNames, m.GetName())
		}
		t.Fatalf("expected %d metrics, got %d: %v", len(expectedNames), len(metrics), gotNames)
	}

	// Check that all expected metrics are present and are Gauge type
	foundNames := make(map[string]bool)
	for _, m := range metrics {
		foundNames[m.GetName()] = true

		// Verify it's a Gauge (not Summary)
		if m.GetGauge() == nil {
			t.Errorf("metric %s should be Gauge type", m.GetName())
		}

		// Check labels are correct including new YACE context labels
		if len(m.GetGauge().GetDataPoints()) > 0 {
			attrs := keyValueToMap(m.GetGauge().GetDataPoints()[0].GetAttributes())
			if attrs["region"] != "us-east-1" {
				t.Errorf("metric %s: expected region=us-east-1, got %s", m.GetName(), attrs["region"])
			}
			if attrs["account_id"] != "123456789012" {
				t.Errorf("metric %s: expected account_id=123456789012, got %s", m.GetName(), attrs["account_id"])
			}
			if attrs["namespace"] != "AWS/EC2" {
				t.Errorf("metric %s: expected namespace=AWS/EC2, got %s", m.GetName(), attrs["namespace"])
			}
			if attrs["name"] != ec2ARN {
				t.Errorf("metric %s: expected name=%s, got %s", m.GetName(), ec2ARN, attrs["name"])
			}
			if attrs["dimension_instance_id"] != "i-1234567890abcdef0" {
				t.Errorf("metric %s: expected dimension_instance_id=i-1234567890abcdef0, got %s", m.GetName(), attrs["dimension_instance_id"])
			}
		}
	}

	for _, name := range expectedNames {
		if !foundNames[name] {
			t.Errorf("expected metric %s not found", name)
		}
	}
}
