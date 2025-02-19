package adapter

import (
	"fmt"

	"px.dev/pxapi/proto/vizierpb"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"px.dev/pxapi/types"
)

const jvmTemplate = `
#px:set max_output_rows_per_table=10000

import px
df = px.DataFrame('jvm_stats', start_time='-%ds')

df.container = df.ctx['container_name']
df.pod = df.ctx['pod']
df.service = df.ctx['service']
df.namespace = df.ctx['namespace']

df.used_heap_size = px.Bytes(df.used_heap_size)
df.total_heap_size = px.Bytes(df.total_heap_size)
df.max_heap_size = px.Bytes(df.max_heap_size)

# Aggregate over each process, k8s_object.
by_upid = df.groupby(['upid','container', 'pod', 'service', 'namespace']).agg(
    young_gc_time_max=('young_gc_time', px.max),
    young_gc_time_min=('young_gc_time', px.min),
    full_gc_time_max=('full_gc_time', px.max),
    full_gc_time_min=('full_gc_time', px.min),
    used_heap_size=('used_heap_size', px.mean),
    total_heap_size=('total_heap_size', px.mean),
    max_heap_size=('max_heap_size', px.mean),
    timestamp=('time_', px.max),
)

# Convert the counter metrics into accumulated values over the window.
by_upid.young_gc_time = by_upid.young_gc_time_max - by_upid.young_gc_time_min
by_upid.full_gc_time = by_upid.full_gc_time_max - by_upid.full_gc_time_min

# Aggregate over each k8s_object.
by_k8s = by_upid.groupby(['container', 'pod', 'service', 'namespace']).agg(
    young_gc_time=('young_gc_time', px.sum),
    full_gc_time=('full_gc_time', px.sum),
    used_heap_size=('used_heap_size', px.sum),
    max_heap_size=('max_heap_size', px.sum),
    total_heap_size=('total_heap_size', px.sum),
    timestamp=('timestamp', px.max),
)
by_k8s.young_gc_time = px.DurationNanos(by_k8s.young_gc_time) / 1000000.0
by_k8s.full_gc_time = px.DurationNanos(by_k8s.full_gc_time) / 1000000.0
by_k8s['time_'] = by_k8s['timestamp']

px.display(by_k8s, 'jvm')
`

var metricMapping = map[string]metricDef{
	"young_gc_time":   {"runtime.jvm.gc.collection", "", "ms", map[string]interface{}{"gc": "young"}},
	"full_gc_time":    {"runtime.jvm.gc.collection", "", "ms", map[string]interface{}{"gc": "full"}},
	"used_heap_size":  {"runtime.jvm.memory.area", "", "bytes", map[string]interface{}{"type": "used", "area": "heap"}},
	"total_heap_size": {"runtime.jvm.memory.area", "", "bytes", map[string]interface{}{"type": "total", "area": "heap"}},
	"max_heap_size":   {"runtime.jvm.memory.area", "", "bytes", map[string]interface{}{"type": "max", "area": "heap"}},
}

type metricDef struct {
	metricName  string
	description string
	unit        string
	attributes  map[string]interface{}
}

type jvm struct {
	clusterName        string
	pixieClusterID     string
	collectIntervalSec int64
	script             string
}

func newJvm(clusterName, pixieClusterID string, collectIntervalSec int64) *jvm {
	return &jvm{clusterName, pixieClusterID, collectIntervalSec, fmt.Sprintf(jvmTemplate, collectIntervalSec)}
}

func (a *jvm) ID() string {
	return "jvm"
}

func (a *jvm) CollectIntervalSec() int64 {
	return a.collectIntervalSec
}

func (a *jvm) Script() string {
	return a.script
}

func (a *jvm) Adapt(rh *ResourceHelper, r *types.Record) ([]*metricpb.ResourceMetrics, error) {
	timestamp := r.GetDatum("time_").(*types.Time64NSValue).Value()
	instrumentationLibraries := make([]*metricpb.InstrumentationLibraryMetrics, len(metricMapping))
	resources := rh.createResources(r, a.clusterName, a.pixieClusterID)
	index := 0
	for metricName, def := range metricMapping {
		value, err := getValueFromJVMMetric(r, metricName)
		if err != nil {
			return nil, err
		}
		instrumentationLibraries[index] = &metricpb.InstrumentationLibraryMetrics{
			InstrumentationLibrary: instrumentationLibrary,
			Metrics: []*metricpb.Metric{
				{
					Name:        def.metricName,
					Description: def.description,
					Unit:        def.unit,
					Data: &metricpb.Metric_Gauge{
						Gauge: &metricpb.Gauge{
							DataPoints: []*metricpb.NumberDataPoint{
								{
									TimeUnixNano: uint64(timestamp.UnixNano()),
									Value:        &metricpb.NumberDataPoint_AsDouble{value},
									Labels:       transformAttributes(def.attributes),
								},
							},
						},
					},
				},
			},
		}
		index++
	}
	return createArrayOfMetrics(resources, instrumentationLibraries), nil
}

func getValueFromJVMMetric(r *types.Record, metricName string) (float64, error) {
	valueDatum := r.GetDatum(metricName)
	var value float64
	if valueDatum.Type() == vizierpb.INT64 {
		value = float64(valueDatum.(*types.Int64Value).Value())
	} else if valueDatum.Type() == vizierpb.FLOAT64 {
		value = valueDatum.(*types.Float64Value).Value()
	} else {
		return 0, fmt.Errorf("unsupported data type for metric %s", metricName)
	}
	return value, nil
}

func transformAttributes(attrs map[string]interface{}) []*commonpb.StringKeyValue {
	stringKeyValues := make([]*commonpb.StringKeyValue, 0)
	for k := range attrs {
		stringKeyValues = append(stringKeyValues, &commonpb.StringKeyValue{
			Key:   k,
			Value: fmt.Sprintf("%v", attrs[k]),
		})
	}
	return stringKeyValues
}
