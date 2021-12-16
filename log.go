package main

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/log"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

type srvConfig struct {
	Debug struct {
		Format string `toml:"format"`
	} `toml:"debug"`
}

func setupLogFormat(ctx context.Context, config srvConfig) {
	if config.Debug.Format == "" {
		config.Debug.Format = log.TextFormat
	}
	if config.Debug.Format == log.TextFormat {
		logrus.StandardLogger().SetFormatter(&logrus.TextFormatter{
			TimestampFormat: log.RFC3339NanoFixed,
			FullTimestamp:   true,
		})
	} else if config.Debug.Format == log.JSONFormat {
		logrus.StandardLogger().SetFormatter(&logrus.JSONFormatter{
			TimestampFormat: log.RFC3339NanoFixed,
		})
	}
}

func OpenShimLog(ctx context.Context, bundle string) io.Writer {
	logPath := filepath.Join(bundle, "log")
	f, err := os.OpenFile(logPath, os.O_RDWR, 0)
	if err == nil {
		return f
	}

	if !os.IsNotExist(err) {
		log.G(ctx).WithError(err).Warn("Error opening shim log")
		return io.Discard
	}
	if err := unix.Mkfifo(logPath, 0600); err != nil {
		log.G(ctx).WithError(err).Warn("Error creating shim fifo")
		return io.Discard
	}

	f, err = os.OpenFile(logPath, os.O_RDWR, 0)
	if err == nil {
		return f
	}

	log.G(ctx).WithError(err).Warn("Could not open fifo after creating it")
	os.Remove(logPath)
	return io.Discard
}

type shimLogSetKey struct{}

func WithShimLog(ctx context.Context, w io.Writer) context.Context {
	if v := ctx.Value(shimLogSetKey{}); v != nil {
		return ctx
	}

	e := log.G(ctx)

	l := logrus.New()
	l.SetLevel(e.Logger.GetLevel())
	l.SetOutput(io.MultiWriter(e.Logger.Out, w))
	l.Hooks = e.Logger.Hooks
	l.SetReportCaller(e.Logger.ReportCaller)

	e2 := e.Dup()
	e2.Logger = l

	ctx = context.WithValue(ctx, shimLogSetKey{}, struct{}{})

	return log.WithLogger(ctx, e2)
}
