package main

import (
	"context"
	"time"

	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
)

func (s *Service) getState(ctx context.Context, unit string, st *execState) error {
	state, err := s.conn.GetAllPropertiesContext(ctx, unit)
	if err != nil {
		return err
	}

	if p := state["ExecMainPID"]; p != nil {
		st.Pid = uint32(p.(uint32))
	}
	if c := state["ExecMainStatus"]; c != nil {
		st.ExitCode = uint32(c.(int32))
	}
	if ts := state["ExecMainExitTimestamp"]; ts != nil {
		st.ExitedAt = time.UnixMicro(int64(ts.(uint64)))
	}
	return nil
}

func toStatus(s string) task.Status {
	switch s {
	case "created":
		return task.StatusCreated
	case "running":
		return task.StatusRunning
	case "pausing":
		return task.StatusPausing
	case "paused":
		return task.StatusPaused
	case "stopped":
		return task.StatusStopped
	default:
		return task.StatusUnknown
	}
}

type execState struct {
	ExitedAt time.Time
	ExitCode uint32
	Pid      uint32
}

type execStartState struct {
	Path       string
	Started    time.Time
	Exited     time.Time
	ExitStatus uint32
	Args       []string
	Pid        uint32
	StatusCode uint32 // This is the systemd status (e.g. "exited")
}

func parseExecStartStatus(ii [][]interface{}, st *execStartState) error {
	if len(ii) == 0 {
		return errdefs.ErrNotFound
	}

	i := ii[0]

	st.Path = i[0].(string)
	st.Args = i[1].([]string)

	if u64 := i[3].(uint64); u64 > 0 {
		st.Started = time.UnixMicro(int64(u64))
	}

	if u64 := i[5].(uint64); u64 > 0 {
		st.Exited = time.UnixMicro(int64(u64))
	}

	st.Pid = i[7].(uint32)
	st.StatusCode = uint32(i[8].(int32))
	st.ExitStatus = uint32(i[9].(int32))

	return nil
}
