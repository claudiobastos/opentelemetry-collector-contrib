// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package loadbalancingexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/loadbalancingexporter"

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/batchpersignal"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/multierr"
)

var _ exporter.Metrics = (*metricExporterImp)(nil)

type metricExporterImp struct {
	loadBalancer loadBalancer
	routingKey   routingKey

	stopped    bool
	shutdownWg sync.WaitGroup
}

func newMetricsExporter(params exporter.CreateSettings, cfg component.Config) (*metricExporterImp, error) {
	exporterFactory := otlpexporter.NewFactory()

	lb, err := newLoadBalancer(params, cfg, func(ctx context.Context, endpoint string) (component.Component, error) {
		oCfg := buildExporterConfig(cfg.(*Config), endpoint)
		return exporterFactory.CreateMetricsExporter(ctx, params, &oCfg)
	})
	if err != nil {
		return nil, err
	}

	metricExporter := metricExporterImp{loadBalancer: lb, routingKey: traceIDRouting}

	switch cfg.(*Config).RoutingKey {
	case "service":
		metricExporter.routingKey = svcRouting
	case "_metric_ID", "":
	default:
		return nil, fmt.Errorf("unsupported routing_key: %s", cfg.(*Config).RoutingKey)
	}
	return &metricExporter, nil

}

func (e *metricExporterImp) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func (e *metricExporterImp) Start(ctx context.Context, host component.Host) error {
	return e.loadBalancer.Start(ctx, host)
}

func (e *metricExporterImp) Shutdown(context.Context) error {
	e.stopped = true
	e.shutdownWg.Wait()
	return nil
}

func (e *metricExporterImp) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	var errs error
	batches := batchpersignal.SplitMetrics(md)
	for _, batch := range batches {
		errs = multierr.Append(errs, e.consumeMetric(ctx, batch))
	}

	return errs
}

func (e *metricExporterImp) consumeMetric(ctx context.Context, md pmetric.Metrics) error {
	var exp component.Component
	routingIds, err := routingIdentifiersFromMetrics(md, e.routingKey)
	if err != nil {
		return err
	}
	for rid := range routingIds {
		endpoint := e.loadBalancer.Endpoint([]byte(rid))
		exp, err = e.loadBalancer.Exporter(endpoint)
		if err != nil {
			return err
		}

		te, ok := exp.(exporter.Metrics)
		if !ok {
			return fmt.Errorf("unable to export metrics, unexpected exporter type: expected exporter.Metrics but got %T", exp)
		}

		start := time.Now()
		err = te.ConsumeMetrics(ctx, md)
		duration := time.Since(start)

		if err == nil {
			_ = stats.RecordWithTags(
				ctx,
				[]tag.Mutator{tag.Upsert(endpointTagKey, endpoint), successTrueMutator},
				mBackendLatency.M(duration.Milliseconds()))
		} else {
			_ = stats.RecordWithTags(
				ctx,
				[]tag.Mutator{tag.Upsert(endpointTagKey, endpoint), successFalseMutator},
				mBackendLatency.M(duration.Milliseconds()))
		}
	}
	return err

}

func routingIdentifiersFromMetrics(md pmetric.Metrics, key routingKey) (map[string]bool, error) {
	ids := make(map[string]bool)

	// no need to test "empty labels"
	// no need to test "empty resources"

	rs := md.ResourceMetrics()
	if rs.Len() == 0 {
		return nil, errors.New("empty resource metrics")
	}

	ils := rs.At(0).ScopeMetrics()
	if ils.Len() == 0 {
		return nil, errors.New("empty scope metrics")
	}

	metrics := ils.At(0).Metrics()
	if metrics.Len() == 0 {
		return nil, errors.New("empty metrics")
	}

	if key == svcRouting {
		for i := 0; i < rs.Len(); i++ {
			svc, ok := rs.At(i).Resource().Attributes().Get("service.name")
			if !ok {
				return nil, errors.New("unable to get metric name")
			}
			ids[svc.Str()] = true
		}
		return ids, nil
	}

	// qual deve ser usado ???
	tid := metrics.At(0).Name()

	ids[string(tid[:])] = true
	return ids, nil
}
