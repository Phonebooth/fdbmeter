package fdbmeter

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	metricsdk "go.opentelemetry.io/otel/sdk/metric"
)

type (
	Metrics struct {
		exporter *prometheus.Exporter
		provider metric.MeterProvider
		meter    metric.Meter

		getStatusFailureCounter   metric.Int64Counter
		getStatusLatencyHistogram metric.Int64Histogram

		observablesLock sync.RWMutex
		observables     map[string][]observable
	}
	observable struct {
		int64Value   int64
		float64Value float64
		attrs        attribute.Set
	}
)

func NewMetrics() (*Metrics, error) {
	var m Metrics
	var err error
	m.exporter, err = prometheus.New(prometheus.WithoutUnits(),
		prometheus.WithoutScopeInfo(),
		prometheus.WithNamespace("fdb"),
		prometheus.WithoutTargetInfo())
	if err != nil {
		return nil, err
	}
	m.provider = metricsdk.NewMeterProvider(metricsdk.WithReader(m.exporter))
	m.meter = m.provider.Meter("fdb/meter")

	if m.getStatusFailureCounter, err = m.meter.Int64Counter(
		"meter_get_status_failure_counter",
		metric.WithUnit("1")); err != nil {
		return nil, err
	}
	if m.getStatusLatencyHistogram, err = m.meter.Int64Histogram(
		"meter_get_status_latency_histogram",
		metric.WithUnit("ms")); err != nil {
		return nil, err
	}

	var stat Status
	indirect := reflect.Indirect(reflect.ValueOf(stat))
	if err := m.initMetrics(indirect.Type(), ""); err != nil {
		return nil, err
	}
	m.observables = make(map[string][]observable)
	return &m, err
}

func (m *Metrics) shutdown(ctx context.Context) error {
	return m.exporter.Shutdown(ctx)
}

func (m *Metrics) notifyStatusTransactFailed(ctx context.Context) {
	m.getStatusFailureCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("err", "transact")))
}

func (m *Metrics) notifyStatusDecodeFailed(ctx context.Context) {
	m.getStatusFailureCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("err", "decode")))
}

func (m *Metrics) notifyGetStatusLatency(ctx context.Context, latency time.Duration) {
	m.getStatusLatencyHistogram.Record(ctx, latency.Milliseconds())
}

func (m *Metrics) notifyStatus(ctx context.Context, status Status) {
	observables := make(map[string][]observable)
	populateObservables(ctx, traversalContext{
		f: reflect.ValueOf(status),
	}, observables)

	m.observablesLock.Lock()
	defer m.observablesLock.Unlock()
	m.observables = observables
}

type traversalContext struct {
	parent     *reflect.Value
	f          reflect.Value
	metricName string
	attrs      []attribute.KeyValue
	tag        reflect.StructTag
}

func populateObservables(ctx context.Context, tctx traversalContext, observables map[string][]observable) {
	switch tctx.f.Kind() {
	case reflect.Struct:
		if tctx.metricName != "" {
			tctx.metricName = tctx.metricName + "_"
		}
	FieldAttrLoop:
		for i := 0; i < tctx.f.Type().NumField(); i++ {
			select {
			case <-ctx.Done():
				return
			default:
				field := tctx.f.Type().Field(i)
				for _, tagItem := range strings.Split(field.Tag.Get("fdbmeter"), ",") {
					if tagItem == "skip" {
						continue FieldAttrLoop
					}
				}
				if field.Type.Kind() == reflect.String {
					v := tctx.f.Field(i).String()
					if tctx.parent != nil {
						switch tctx.parent.Kind() {
						case reflect.Slice, reflect.Map:
							tctx.attrs = append(tctx.attrs, attribute.String(field.Tag.Get("json"), v))
							continue
						}
					}
					if strings.Contains(field.Tag.Get("fdbmeter"), "attr") {
						tctx.attrs = append(tctx.attrs, attribute.String(field.Tag.Get("json"), v))
					}
				}
			}
		}
	FieldObserveLoop:
		for i := 0; i < tctx.f.Type().NumField(); i++ {
			select {
			case <-ctx.Done():
				return
			default:
				field := tctx.f.Type().Field(i)
				for _, tagItem := range strings.Split(field.Tag.Get("fdbmeter"), ",") {
					if tagItem == "skip" {
						continue FieldObserveLoop
					}
				}
				populateObservables(ctx, traversalContext{
					parent:     tctx.parent,
					f:          tctx.f.Field(i),
					metricName: tctx.metricName + field.Tag.Get("json"),
					attrs:      tctx.attrs,
					tag:        field.Tag,
				}, observables)
			}
		}
	case reflect.Slice:
		for _, tagItem := range strings.Split(tctx.tag.Get("fdbmeter"), ",") {
			if tagItem == "skip" {
				return
			}
		}
		elems, ok := tctx.f.Interface().([]any)
		if ok {
			for _, elem := range elems {
				select {
				case <-ctx.Done():
					return
				default:
					populateObservables(ctx, traversalContext{
						parent:     &tctx.f,
						f:          reflect.ValueOf(elem),
						metricName: tctx.metricName,
						attrs:      tctx.attrs,
						tag:        tctx.tag,
					}, observables)
				}
			}
		}
	case reflect.Map:
		var attrKey = "key"
		fdbmeterTagItems := strings.Split(tctx.tag.Get("fdbmeter"), ",")
		for _, tagItem := range fdbmeterTagItems {
			if tagItem == "skip" {
				return
			}
			if strings.HasPrefix(tagItem, "key=") {
				attrKey = strings.TrimPrefix(tagItem, "key=")
			}
		}
		for r := tctx.f.MapRange(); r.Next(); {
			select {
			case <-ctx.Done():
				return
			default:
				switch r.Key().Kind() {
				case reflect.String:
					tctx.attrs = append(tctx.attrs, attribute.String(attrKey, r.Key().String()))
				}
				populateObservables(ctx, traversalContext{
					parent:     &tctx.f,
					f:          r.Value(),
					metricName: tctx.metricName,
					attrs:      tctx.attrs,
					tag:        tctx.tag,
				}, observables)
			}
		}
	case reflect.Bool:
		var v int64
		if tctx.f.Bool() {
			v = 1
		}
		observables[tctx.metricName] = append(observables[tctx.metricName], observable{
			int64Value: v,
			attrs:      attribute.NewSet(tctx.attrs...),
		})
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		observables[tctx.metricName] = append(observables[tctx.metricName], observable{
			int64Value: tctx.f.Int(),
			attrs:      attribute.NewSet(tctx.attrs...),
		})
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		observables[tctx.metricName] = append(observables[tctx.metricName], observable{
			int64Value: int64(tctx.f.Uint()),
			attrs:      attribute.NewSet(tctx.attrs...),
		})
	case reflect.Float32, reflect.Float64:
		observables[tctx.metricName] = append(observables[tctx.metricName], observable{
			float64Value: tctx.f.Float(),
			attrs:        attribute.NewSet(tctx.attrs...),
		})
	}
}

func (m *Metrics) initMetrics(f reflect.Type, metricName string) error {
	switch f.Kind() {
	case reflect.Struct:
		if metricName != "" {
			metricName = metricName + "_"
		}
	FieldMetricsLoop:
		for i := 0; i < f.NumField(); i++ {
			field := f.Field(i)

			for _, tagItem := range strings.Split(field.Tag.Get("fdbmeter"), ",") {
				if tagItem == "skip" {
					continue FieldMetricsLoop
				}
			}

			if err := m.initMetrics(field.Type, metricName+field.Tag.Get("json")); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice, reflect.Map:
		return m.initMetrics(f.Elem(), metricName)
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		_, err := m.meter.Int64ObservableGauge(
			metricName,
			metric.WithUnit("1"),
			metric.WithInt64Callback(func(ctx context.Context, observer metric.Int64Observer) error {
				m.observablesLock.RLock()
				defer m.observablesLock.RUnlock()
				for _, o := range m.observables[metricName] {
					observer.Observe(o.int64Value, metric.WithAttributeSet(o.attrs))
				}
				return nil
			}),
		)
		return err
	case reflect.Float32, reflect.Float64:
		_, err := m.meter.Float64ObservableGauge(
			metricName,
			metric.WithUnit("1"),
			metric.WithFloat64Callback(func(ctx context.Context, observer metric.Float64Observer) error {
				m.observablesLock.RLock()
				defer m.observablesLock.RUnlock()
				for _, o := range m.observables[metricName] {
					observer.Observe(o.float64Value, metric.WithAttributeSet(o.attrs))
				}
				return nil
			}),
		)
		return err
	default:
		return nil
	}
}