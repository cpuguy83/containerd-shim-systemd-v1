package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/tracing"
	"github.com/containerd/ttrpc"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
	return otel.Tracer("").Start(ctx, name, opts...)
}

var (
	RPCSystemAttr = semconv.RPCSystemKey.String("ttrpc")
)

func UnaryServerInterceptor(ctx context.Context, u ttrpc.Unmarshaler, info *ttrpc.UnaryServerInfo, m ttrpc.Method) (interface{}, error) {
	if md, ok := ttrpc.GetMetadata(ctx); ok {
		ctx = otel.GetTextMapPropagator().Extract(ctx, &carrier{md})
	}

	ctx, span := StartSpan(ctx, info.FullMethod,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(RPCSystemAttr),
	)

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("ttrpc.method", info.FullMethod))

	span.AddEvent("sent")
	resp, err := m(ctx, u)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	span.AddEvent("received")

	span.End()

	return resp, err
}

func UnaryClientInterceptor(ctx context.Context, req *ttrpc.Request, resp *ttrpc.Response, info *ttrpc.UnaryClientInfo, invoker ttrpc.Invoker) error {
	ctx, span := StartSpan(ctx, info.FullMethod,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			RPCSystemAttr,
			attribute.String("rpc.ttrpc.service", req.Service),
			attribute.String("rpc.ttrpc.method", req.Method),
		),
	)

	span.AddEvent("sent", trace.WithAttributes(
		attribute.Int("message.payload.size", len(req.Payload)),
	))

	if span.IsRecording() {
		md, ok := ttrpc.GetMetadata(ctx)
		if !ok {
			md = ttrpc.MD{}
		}
		otel.GetTextMapPropagator().Inject(ctx, &carrier{md})
		ctx = ttrpc.WithMetadata(ctx, md)
	}

	err := invoker(ctx, req, resp)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}

	span.AddEvent("received", trace.WithAttributes(
		attribute.Int("message.payload.size", len(resp.Payload)),
	))
	if resp.Status != nil {
		span.SetAttributes(
			attribute.Int("rpc.ttrpc.response.status", int(resp.Status.Code)),
		)
	}

	span.End()
	return err
}

type carrier struct {
	md ttrpc.MD
}

func (c *carrier) Get(key string) string {
	v, _ := c.md.Get(key)
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func (c *carrier) Set(key, value string) {
	c.md.Set(key, value)
}

func (c *carrier) Keys() []string {
	keys := make([]string, 0, len(c.md))
	for k := range c.md {
		keys = append(keys, k)
	}
	return keys
}
