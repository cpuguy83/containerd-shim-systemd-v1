package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/tracing"
	"github.com/containerd/ttrpc"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

const (
	cIDAttr = "container.id"
	eIDAttr = "exec.id"
	nsAttr  = "ns"
)

type TraceConfig struct {
	Endpoint   string
	SampleRate float64
	Insecure   bool
}

func (c TraceConfig) StringFlags() string {
	return fmt.Sprintf("--trace-endpoint=%s --trace-sample-rate=%f --trace-insecure=%t", c.Endpoint, c.SampleRate, c.Insecure)
}

func TraceFlags(fl *flag.FlagSet) *TraceConfig {
	var cfg TraceConfig

	fl.StringVar(&cfg.Endpoint, "trace-endpoint", "", "set the otlp endpoint for the agent to send trace data to")
	fl.Float64Var(&cfg.SampleRate, "trace-sample-rate", 1.0, "set the sampling rate for the trace exporter")
	fl.BoolVar(&cfg.Insecure, "trace-insecure", false, "allow traces to be sent to insecure endpoint")

	return &cfg
}

func ConfigureTracing(ctx context.Context, cfg *TraceConfig) (func(context.Context), error) {
	if cfg.Endpoint == "" {
		return func(context.Context) {}, nil
	}

	opts := []grpc.DialOption{
		grpc.WithBlock(),
	}
	if cfg.Insecure {
		opts = append(opts, grpc.WithInsecure())
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithDialOption(opts...),
	)
	if err != nil {
		return nil, fmt.Errorf("error setting up otel exporter")
	}

	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceNameKey.String(shimName)))
	if err != nil {
		return nil, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.SampleRate)),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(exp)),
	)

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	logrus.AddHook(tracing.NewLogrusHook())

	return func(ctx context.Context) {
		if err := provider.Shutdown(ctx); err != nil {
			log.G(ctx).WithError(err).Error("error shutting down tracing")
		}
	}, nil
}

func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return otel.Tracer(shimName).Start(ctx, name, opts...)
}

func traceInterceptor(ctx context.Context, u ttrpc.Unmarshaler, i *ttrpc.UnaryServerInfo, m ttrpc.Method) (interface{}, error) {
	if ns, _ := namespaces.Namespace(ctx); ns != "" {
		namespaces.WithNamespace(ctx, ns)
	}
	ctx, span := StartSpan(ctx, i.FullMethod,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			semconv.RPCSystemKey.String("ttrpc"),
		),
	)
	defer span.End()

	ret, err := m(ctx, u)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	return ret, err
}
