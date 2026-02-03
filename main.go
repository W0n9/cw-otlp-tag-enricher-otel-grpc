package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/matttproud/golang_protobuf_extensions/v2/pbutil"
	"github.com/prometheus-community/yet-another-cloudwatch-exporter/pkg/clients/tagging"
	clientsv2 "github.com/prometheus-community/yet-another-cloudwatch-exporter/pkg/clients/v2"
	"github.com/prometheus-community/yet-another-cloudwatch-exporter/pkg/config"
	"github.com/prometheus-community/yet-another-cloudwatch-exporter/pkg/job/maxdimassociator"
	"github.com/prometheus-community/yet-another-cloudwatch-exporter/pkg/model"
	"github.com/prometheus-community/yet-another-cloudwatch-exporter/pkg/promutil"
	metricsservicepb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const cacheFile = "cache"

func main() {
	lambda.Start(lambdaHandler)
}

func lambdaHandler(ctx context.Context, request events.KinesisFirehoseEvent) (interface{}, error) {
	logger := newLogger(os.Getenv("LOG_LEVEL"))
	region := aws.String(os.Getenv("AWS_REGION"))

	continueOnResourceFailure := envBool("CONTINUE_ON_RESOURCE_FAILURE", true)
	continueOnExportFailure := envBool("CONTINUE_ON_EXPORT_FAILURE", true)
	fileCacheEnabled := envBool("FILE_CACHE_ENABLED", true)
	fileCacheExpiration := envDuration("FILE_CACHE_EXPIRATION", 1*time.Hour, logger)
	fileCachePath := envString("FILE_CACHE_PATH", "/tmp")
	staticLabels, err := parseStaticLabels(os.Getenv("STATIC_LABELS"))
	if err != nil {
		logger.Error("Failed to parse STATIC_LABELS", "error", err)
	}
	defaultLabels := envBool("DEFAULT_LABELS", false)
	labelsSnakeCase := envBool("LABELS_SNAKE_CASE", true)
	exportedTags, err := parseExportedTags(os.Getenv("EXPORTED_TAGS_ON_METRICS"))
	if err != nil {
		logger.Error("Failed to parse EXPORTED_TAGS_ON_METRICS", "error", err)
	}
	outputMode := strings.ToLower(envString("FIREHOSE_OUTPUT_MODE", "pass_through"))
	yaceCompatMode := envBool("YACE_COMPAT_MODE", false)
	yaceCompatStats, err := parseYACEStats(os.Getenv("YACE_COMPAT_STATS"))
	if err != nil {
		logger.Error("Failed to parse YACE_COMPAT_STATS", "error", err)
		// Use defaults on error
		yaceCompatStats, _ = parseYACEStats("")
	}

	resourcesPerNamespace := make(map[string][]*model.TaggedResource)
	associatorsPerNamespace := make(map[string]maxdimassociator.Associator)
	responseRecords := make([]events.KinesisFirehoseResponseRecord, 0, len(request.Records))

	cache, err := clientsv2.NewFactory(logger, model.JobsConfig{
		DiscoveryJobs: []model.DiscoveryJob{
			{
				Regions: []string{*region},
				Roles:   []model.Role{{}},
			},
		},
	}, false)
	if err != nil {
		logger.Error("Failed to create a new cache client", "error", err)
		return nil, err
	}
	cache.Refresh()
	clientTag := cache.GetTaggingClient(*region, model.Role{}, 5)

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	insecureConn := envBool("OTEL_EXPORTER_OTLP_INSECURE", true)
	exportTimeout := envDuration("OTEL_EXPORTER_OTLP_TIMEOUT", 5*time.Second, logger)

	var grpcConn *grpc.ClientConn
	if endpoint != "" {
		grpcConn, err = newGRPCConn(endpoint, insecureConn, exportTimeout)
		if err != nil {
			logger.Error("Failed to create OTLP gRPC connection", "error", err)
			if !continueOnExportFailure {
				return nil, err
			}
		}
	}
	if grpcConn != nil {
		defer grpcConn.Close()
	}

	var grpcClient metricsservicepb.MetricsServiceClient
	if grpcConn != nil {
		grpcClient = metricsservicepb.NewMetricsServiceClient(grpcConn)
	}

	for _, record := range request.Records {
		expMetricsReqs, err := rawDataIntoRequests(record.Data)
		if err != nil {
			logger.Error("Failed to decode record data", "error", err)
			if !continueOnExportFailure {
				return nil, err
			}
			responseRecords = append(responseRecords, passThroughRecord(record))
			continue
		}

		if err := enhanceRequests(
			logger,
			fileCachePath,
			continueOnResourceFailure,
			expMetricsReqs,
			resourcesPerNamespace,
			associatorsPerNamespace,
			region,
			clientTag,
			fileCacheExpiration,
			fileCacheEnabled,
			staticLabels,
			defaultLabels,
			labelsSnakeCase,
			exportedTags,
			yaceCompatMode,
			yaceCompatStats,
		); err != nil {
			logger.Error("Failed to enhance record data", "error", err)
			if !continueOnResourceFailure {
				return nil, err
			}
		}

		if grpcClient != nil {
			err = exportRequests(ctx, grpcClient, expMetricsReqs, exportTimeout)
			if err != nil {
				logger.Error("Failed to export OTLP metrics", "error", err)
				if !continueOnExportFailure {
					return nil, err
				}
			}
		}

		var responseData []byte
		if outputMode == "enhanced" {
			responseData, err = requestsIntoRawData(expMetricsReqs)
			if err != nil {
				logger.Error("Failed to encode enhanced metrics", "error", err)
				if !continueOnExportFailure {
					return nil, err
				}
				responseRecords = append(responseRecords, passThroughRecord(record))
				continue
			}
		} else {
			responseData = record.Data
		}

		responseRecords = append(responseRecords, buildResponseRecord(record.RecordID, responseData))
	}

	return events.KinesisFirehoseResponse{
		Records: responseRecords,
	}, nil
}

func enhanceRequests(
	logger *slog.Logger,
	fileCachePath string,
	continueOnResourceFailure bool,
	expMetricsReqs []*metricsservicepb.ExportMetricsServiceRequest,
	resourceCache map[string][]*model.TaggedResource,
	associatorCache map[string]maxdimassociator.Associator,
	region *string,
	client tagging.Client,
	fileCacheExpiration time.Duration,
	fileCacheEnabled bool,
	staticLabels map[string]string,
	defaultLabels bool,
	labelsSnakeCase bool,
	exportedTags []string,
	yaceCompatMode bool,
	yaceCompatStats map[string]bool,
) error {
	for _, req := range expMetricsReqs {
		for _, rm := range req.GetResourceMetrics() {
			// Extract account_id and region from resource attributes
			accountID, resourceRegion := extractResourceAttributes(rm)
			// Use resource region if available, otherwise fall back to Lambda region
			effectiveRegion := resourceRegion
			if effectiveRegion == "" && region != nil {
				effectiveRegion = *region
			}

			for smIdx, sm := range rm.GetScopeMetrics() {
				var newMetrics []*metricspb.Metric
				for _, metric := range sm.GetMetrics() {
					switch t := metric.Data.(type) {
					case *metricspb.Metric_Summary:
						for _, dp := range t.Summary.GetDataPoints() {
							attrs := dp.GetAttributes()
							cwm := buildCloudWatchMetricFromKeyValues(attrs)
							if cwm.MetricName == "" || cwm.Namespace == "" {
								logger.Debug("Metric name or namespace is missing, skipping tags enrichment", "namespace", cwm.Namespace, "metric", cwm.MetricName)
								continue
							}
							svc := config.SupportedServices.GetService(cwm.Namespace)
							if svc == nil {
								logger.Debug("Unsupported namespace, skipping tags enrichment", "namespace", cwm.Namespace, "metric", cwm.MetricName)
								continue
							}

							if _, ok := resourceCache[cwm.Namespace]; !ok {
								resources, err := getOrCacheResources(
									logger,
									client,
									fileCachePath,
									cwm.Namespace,
									region,
									fileCacheExpiration,
									fileCacheEnabled,
								)
								if err != nil && err != tagging.ErrExpectedToFindResources {
									if continueOnResourceFailure {
										logger.Error("Failed to get resources for namespace", "namespace", cwm.Namespace, "error", err)
										continue
									}
									return err
								}
								resourceCache[cwm.Namespace] = resources
							}

							asc, ok := associatorCache[cwm.Namespace]
							if !ok {
								asc = maxdimassociator.NewAssociator(logger, svc.ToModelDimensionsRegexp(), resourceCache[cwm.Namespace])
								associatorCache[cwm.Namespace] = asc
							}

							r, skip := asc.AssociateMetricToResource(cwm)
							yaceLabels := buildYACELabelsKeyValue(logger, cwm, r, skip, staticLabels, defaultLabels, labelsSnakeCase, exportedTags, effectiveRegion, accountID)

							if yaceCompatMode {
								// Convert Summary to multiple Gauge metrics for YACE compatibility
								gauges := summaryToGauges(cwm, dp, yaceLabels, yaceCompatStats)
								newMetrics = append(newMetrics, gauges...)
							} else {
								// Original behavior: update metric name and attributes in place
								statistic := attrValue(attrs, "Statistic")
								if statistic == "" {
									statistic = attrValue(attrs, "statistic")
								}
								metric.Name = promutil.BuildMetricName(cwm.Namespace, cwm.MetricName, statistic)
								dp.Attributes = yaceLabels
							}
						}
					default:
						logger.Debug("Unsupported metric type", "type", fmt.Sprintf("%T", t))
						if yaceCompatMode {
							// Keep non-Summary metrics as-is in YACE compat mode
							newMetrics = append(newMetrics, metric)
						}
					}
				}

				// Replace metrics with converted gauges when in YACE compat mode
				if yaceCompatMode {
					rm.ScopeMetrics[smIdx].Metrics = newMetrics
				}
			}
		}
	}

	return nil
}

// attrValue returns the string value for key in OTLP 1.0 KeyValue attributes, or "" if not found.
func attrValue(attrs []*commonpb.KeyValue, key string) string {
	for _, a := range attrs {
		if a != nil && a.GetKey() == key {
			if v := a.GetValue(); v != nil {
				return v.GetStringValue()
			}
			return ""
		}
	}
	return ""
}

// extractResourceAttributes extracts cloud.account.id and cloud.region from OTLP Resource attributes.
// CloudWatch Metric Streams includes these in the resource attributes.
func extractResourceAttributes(rm *metricspb.ResourceMetrics) (accountID, resourceRegion string) {
	if rm == nil || rm.GetResource() == nil {
		return "", ""
	}
	for _, attr := range rm.GetResource().GetAttributes() {
		if attr == nil {
			continue
		}
		switch attr.GetKey() {
		case "cloud.account.id":
			if v := attr.GetValue(); v != nil {
				accountID = v.GetStringValue()
			}
		case "cloud.region":
			if v := attr.GetValue(); v != nil {
				resourceRegion = v.GetStringValue()
			}
		}
	}
	return accountID, resourceRegion
}

// buildYACELabelsKeyValue builds OTLP 1.0 KeyValue attributes per YACE: region, account_id, name, dimension_*, tag_*, custom_tag_*.
func buildYACELabelsKeyValue(
	logger *slog.Logger,
	cwm *model.Metric,
	r *model.TaggedResource,
	skip bool,
	staticLabels map[string]string,
	defaultLabels bool,
	labelsSnakeCase bool,
	exportedTags []string,
	region string,
	accountID string,
) []*commonpb.KeyValue {
	var out []*commonpb.KeyValue
	strVal := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}

	// Add region and account_id labels (YACE context labels)
	if region != "" {
		out = append(out, &commonpb.KeyValue{Key: "region", Value: strVal(region)})
	}
	if accountID != "" {
		out = append(out, &commonpb.KeyValue{Key: "account_id", Value: strVal(accountID)})
	}

	// Add namespace label for dashboard compatibility
	if cwm.Namespace != "" {
		out = append(out, &commonpb.KeyValue{Key: "namespace", Value: strVal(cwm.Namespace)})
	}

	nameVal := "global"
	if r != nil && !skip {
		nameVal = r.ARN
	}
	out = append(out, &commonpb.KeyValue{Key: "name", Value: strVal(nameVal)})

	for _, dim := range cwm.Dimensions {
		ok, promTag := promutil.PromStringTag(dim.Name, labelsSnakeCase)
		if !ok {
			logger.Warn("dimension name is an invalid prometheus label name", "dimension", dim.Name)
			continue
		}
		out = append(out, &commonpb.KeyValue{Key: "dimension_" + promTag, Value: strVal(dim.Value)})
	}

	if r != nil && !skip {
		tagsToExport := r.Tags
		if len(exportedTags) > 0 {
			tagsToExport = r.MetricTags(exportedTags)
		}
		for _, tag := range tagsToExport {
			ok, promTag := promutil.PromStringTag(tag.Key, labelsSnakeCase)
			if !ok {
				logger.Warn("metric tag name is an invalid prometheus label name", "tag", tag.Key)
				continue
			}
			out = append(out, &commonpb.KeyValue{Key: "tag_" + promTag, Value: strVal(tag.Value)})
		}
	}

	if defaultLabels || (r != nil && !skip) {
		for k, v := range staticLabels {
			ok, promTag := promutil.PromStringTag(k, labelsSnakeCase)
			if !ok {
				logger.Warn("custom tag name is an invalid prometheus label name", "tag", k)
				continue
			}
			out = append(out, &commonpb.KeyValue{Key: "custom_tag_" + promTag, Value: strVal(v)})
		}
	}

	return out
}

// defaultYACEStats is the default set of statistics to export in YACE compatibility mode.
var defaultYACEStats = []string{"Maximum", "Minimum", "Average", "Sum", "SampleCount"}

// quantileToStatistic maps a quantile value to a YACE-compatible statistic name.
func quantileToStatistic(q float64) string {
	switch q {
	case 0.0:
		return "Minimum"
	case 1.0:
		return "Maximum"
	default:
		// Convert quantile to percentile (e.g., 0.95 -> p95, 0.999 -> p99_9)
		pct := q * 100
		if pct == float64(int(pct)) {
			return fmt.Sprintf("p%.0f", pct)
		}
		// Handle fractional percentiles like p99.9
		return fmt.Sprintf("p%s", strings.ReplaceAll(fmt.Sprintf("%.1f", pct), ".", "_"))
	}
}

// newGauge creates a new OTLP Gauge metric with a single data point.
func newGauge(name string, value float64, timestampNano uint64, startTimeNano uint64, attrs []*commonpb.KeyValue) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Gauge{
			Gauge: &metricspb.Gauge{
				DataPoints: []*metricspb.NumberDataPoint{{
					Attributes:        attrs,
					StartTimeUnixNano: startTimeNano,
					TimeUnixNano:      timestampNano,
					Value:             &metricspb.NumberDataPoint_AsDouble{AsDouble: value},
				}},
			},
		},
	}
}

// summaryToGauges converts a Summary metric to multiple Gauge metrics for YACE compatibility.
// It extracts SampleCount, Sum, Average, Minimum, Maximum, and percentiles as separate gauges.
func summaryToGauges(
	cwm *model.Metric,
	dp *metricspb.SummaryDataPoint,
	attrs []*commonpb.KeyValue,
	enabledStats map[string]bool,
) []*metricspb.Metric {
	var gauges []*metricspb.Metric
	ts := dp.GetTimeUnixNano()
	startTs := dp.GetStartTimeUnixNano()
	count := dp.GetCount()
	sum := dp.GetSum()

	// SampleCount
	if enabledStats["SampleCount"] {
		gauges = append(gauges, newGauge(
			promutil.BuildMetricName(cwm.Namespace, cwm.MetricName, "SampleCount"),
			float64(count), ts, startTs, attrs))
	}

	// Sum
	if enabledStats["Sum"] {
		gauges = append(gauges, newGauge(
			promutil.BuildMetricName(cwm.Namespace, cwm.MetricName, "Sum"),
			sum, ts, startTs, attrs))
	}

	// Average (calculated from sum/count)
	if enabledStats["Average"] && count > 0 {
		gauges = append(gauges, newGauge(
			promutil.BuildMetricName(cwm.Namespace, cwm.MetricName, "Average"),
			sum/float64(count), ts, startTs, attrs))
	}

	// Quantiles -> Minimum, Maximum, percentiles
	for _, qv := range dp.GetQuantileValues() {
		stat := quantileToStatistic(qv.GetQuantile())
		if enabledStats[stat] {
			gauges = append(gauges, newGauge(
				promutil.BuildMetricName(cwm.Namespace, cwm.MetricName, stat),
				qv.GetValue(), ts, startTs, attrs))
		}
	}

	return gauges
}

// parseYACEStats parses YACE_COMPAT_STATS environment variable and returns a map of enabled statistics.
func parseYACEStats(env string) (map[string]bool, error) {
	enabled := make(map[string]bool)
	if env == "" {
		// Default: enable all standard statistics
		for _, s := range defaultYACEStats {
			enabled[s] = true
		}
		return enabled, nil
	}

	var stats []string
	if err := json.Unmarshal([]byte(env), &stats); err != nil {
		return nil, err
	}
	for _, s := range stats {
		enabled[s] = true
	}
	return enabled, nil
}

func parseExportedTags(env string) ([]string, error) {
	if env == "" {
		return nil, nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(env), &tags); err != nil {
		return nil, err
	}
	return tags, nil
}

func getOrCacheResources(
	logger *slog.Logger,
	client tagging.Client,
	fileCachePath,
	namespace string,
	region *string,
	cacheExpiration time.Duration,
	cacheEnabled bool,
) ([]*model.TaggedResource, error) {
	if !cacheEnabled {
		return retrieveResources(namespace, region, client)
	}

	filePath := fileCachePath + "/" + cacheFile + "-" + strings.ReplaceAll(namespace, "/", "-")
	f, err := os.Open(filePath)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	var isExpired bool
	if !os.IsNotExist(err) {
		fs, err := f.Stat()
		if err != nil {
			return nil, err
		}
		isExpired = fs.ModTime().Add(cacheExpiration).Before(time.Now())
	}

	if os.IsNotExist(err) || isExpired {
		logger.Debug("refreshing resource cache", "namespace", namespace)
		resources, err := retrieveResources(namespace, region, client)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(resources)
		if err != nil {
			return nil, err
		}

		f, err := os.Create(filePath)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		if _, err := f.Write(b); err != nil {
			return nil, err
		}

		return resources, nil
	}

	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	var resources []*model.TaggedResource
	if err := json.Unmarshal(b, &resources); err != nil {
		return nil, err
	}
	logger.Debug("loaded resources from cache", "namespace", namespace, "count", len(resources))
	return resources, nil
}

func retrieveResources(namespace string, region *string, client tagging.Client) ([]*model.TaggedResource, error) {
	resources, err := client.GetResources(context.Background(), model.DiscoveryJob{
		Namespace: namespace,
	}, *region)
	if err != nil && err != tagging.ErrExpectedToFindResources {
		return nil, err
	}

	return resources, nil
}

// buildCloudWatchMetricFromKeyValues parses OTLP 1.0 data point attributes: Namespace, MetricName,
// and Dimensions (key "Dimensions" with kvlist_value in AWS CloudWatch 1.0.0 format).
func buildCloudWatchMetricFromKeyValues(attrs []*commonpb.KeyValue) *model.Metric {
	cwm := &model.Metric{}
	for _, a := range attrs {
		if a == nil {
			continue
		}
		k := a.GetKey()
		v := a.GetValue()
		if v == nil {
			continue
		}
		switch k {
		case "MetricName":
			cwm.MetricName = v.GetStringValue()
		case "Namespace":
			cwm.Namespace = v.GetStringValue()
		case "Dimensions":
			if kvlist := v.GetKvlistValue(); kvlist != nil {
				for _, kv := range kvlist.GetValues() {
					if kv != nil && kv.GetValue() != nil {
						cwm.Dimensions = append(cwm.Dimensions, model.Dimension{
							Name:  kv.GetKey(),
							Value: kv.GetValue().GetStringValue(),
						})
					}
				}
			}
		}
	}
	return cwm
}

func rawDataIntoRequests(input []byte) ([]*metricsservicepb.ExportMetricsServiceRequest, error) {
	var requests []*metricsservicepb.ExportMetricsServiceRequest
	r := bytes.NewBuffer(input)
	for {
		rm := &metricsservicepb.ExportMetricsServiceRequest{}
		_, err := pbutil.ReadDelimited(r, rm)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		requests = append(requests, rm)
	}
	return requests, nil
}

func requestsIntoRawData(reqs []*metricsservicepb.ExportMetricsServiceRequest) ([]byte, error) {
	var b bytes.Buffer
	for _, r := range reqs {
		if _, err := pbutil.WriteDelimited(&b, r); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

func exportRequests(
	ctx context.Context,
	client metricsservicepb.MetricsServiceClient,
	reqs []*metricsservicepb.ExportMetricsServiceRequest,
	timeout time.Duration,
) error {
	for _, r := range reqs {
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		_, err := client.Export(reqCtx, r)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func newGRPCConn(endpoint string, insecureConn bool, timeout time.Duration) (*grpc.ClientConn, error) {
	dialCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var creds credentials.TransportCredentials
	if insecureConn {
		creds = insecure.NewCredentials()
	} else {
		creds = credentials.NewClientTLSFromCert(nil, "")
	}

	return grpc.DialContext(
		dialCtx,
		endpoint,
		grpc.WithTransportCredentials(creds),
		grpc.WithBlock(),
	)
}

func buildResponseRecord(recordID string, data []byte) events.KinesisFirehoseResponseRecord {
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(data)))
	base64.StdEncoding.Encode(encoded, data)
	return events.KinesisFirehoseResponseRecord{
		RecordID: recordID,
		Result:   "Ok",
		Data:     encoded,
	}
}

func passThroughRecord(record events.KinesisFirehoseEventRecord) events.KinesisFirehoseResponseRecord {
	return buildResponseRecord(record.RecordID, record.Data)
}

func parseStaticLabels(staticLabelsEnv string) (map[string]string, error) {
	staticLabels := make(map[string]string)
	if staticLabelsEnv == "" {
		return staticLabels, nil
	}

	var rawLabels []string
	if err := json.Unmarshal([]byte(staticLabelsEnv), &rawLabels); err != nil {
		return staticLabels, err
	}

	for _, label := range rawLabels {
		if label == "" {
			return staticLabels, errors.New("STATIC_LABELS contains empty string")
		}
		if !strings.Contains(label, "=") {
			return staticLabels, errors.New("STATIC_LABELS contains string that is not a key=value pair")
		}
		key, value := strings.Split(label, "=")[0], strings.SplitN(label, "=", 2)[1]
		staticLabels[key] = value
	}

	return staticLabels, nil
}

func envBool(key string, defaultValue bool) bool {
	value := strings.ToLower(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	return value == "true"
}

func envString(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func envDuration(key string, defaultValue time.Duration, logger *slog.Logger) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		} else {
			logger.Error("Failed to parse duration value, using default", "key", key, "error", err)
		}
	}
	return defaultValue
}

func newLogger(level string) *slog.Logger {
	logLevel := slog.LevelInfo
	if strings.ToLower(level) == "debug" {
		logLevel = slog.LevelDebug
	}
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	})
	return slog.New(handler)
}
