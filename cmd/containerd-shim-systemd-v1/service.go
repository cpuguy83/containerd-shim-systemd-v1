package main

import (
	"context"
	"net"

	"github.com/containerd/containerd/log"
	shimapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/ttrpc"
	"github.com/coreos/go-systemd/v22/daemon"
)

func NewService(ts shimapi.TaskService) (*Service, error) {
	s, err := ttrpc.NewServer(ttrpc.WithServerHandshaker(ttrpc.UnixSocketRequireSameUser()))
	if err != nil {
		return nil, err
	}

	shimapi.RegisterTaskService(s, ts)

	return &Service{
		srv: s,
	}, nil
}

type Service struct {
	srv *ttrpc.Server
}

func (s *Service) Serve(ctx context.Context, l net.Listener) error {
	daemon.SdNotify(false, daemon.SdNotifyReady)
	log.G(ctx).Info("Serving")
	return s.srv.Serve(ctx, l)
}

func (s *Service) Close() error {
	return s.srv.Close()
}
