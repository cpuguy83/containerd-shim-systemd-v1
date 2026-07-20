package main

import (
	"context"
	"errors"
	"path"
	"testing"
	"time"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

func TestServiceWaitReconcilesExitState(t *testing.T) {
	const (
		namespace = "test"
		id        = "container"
	)

	t.Run("nonterminal disk state falls back to systemd before waiting", func(t *testing.T) {
		p := newWaitProcess(id, pState{Pid: 42, Status: "running"})
		p.loadExitStateFn = func(p *fakeProcess) error {
			p.setState(pState{
				Pid:      42,
				ExitCode: 9,
				ExitedAt: time.Now(),
				Status:   "failed",
			})
			return nil
		}
		service := newWaitService(t, namespace, id, p)

		response, err := service.Wait(
			namespaces.WithNamespace(context.Background(), namespace),
			&taskapi.WaitRequest{ID: id},
		)
		if err != nil {
			t.Fatalf("wait for process: %v", err)
		}
		if response.ExitStatus != 9 {
			t.Fatalf("exit status = %d, want 9", response.ExitStatus)
		}
		if got := p.LoadExitStateCalls(); got != 1 {
			t.Fatalf("systemd exit-state loads = %d, want 1", got)
		}
	})

	t.Run("terminal disk state does not fall back to systemd", func(t *testing.T) {
		p := newWaitProcess(id, pState{Pid: 42, Status: "running"})
		p.loadStateFn = func(p *fakeProcess) error {
			p.setState(pState{
				Pid:      42,
				ExitCode: 7,
				ExitedAt: time.Now(),
				Status:   "failed",
			})
			return nil
		}
		service := newWaitService(t, namespace, id, p)

		response, err := service.Wait(
			namespaces.WithNamespace(context.Background(), namespace),
			&taskapi.WaitRequest{ID: id},
		)
		if err != nil {
			t.Fatalf("wait for process: %v", err)
		}
		if response.ExitStatus != 7 {
			t.Fatalf("exit status = %d, want 7", response.ExitStatus)
		}
		if got := p.LoadExitStateCalls(); got != 0 {
			t.Fatalf("systemd exit-state loads = %d, want 0", got)
		}
	})
}

type waitProcess struct {
	*fakeProcess
}

func newWaitProcess(name string, state pState) *waitProcess {
	p := &waitProcess{fakeProcess: &fakeProcess{name: name}}
	p.setState(state)
	return p
}

func (p *waitProcess) Wait(context.Context) (pState, error) {
	state := p.ProcessState()
	if !state.Exited() {
		return pState{}, errors.New("wait called before exit state was reconciled")
	}
	return state, nil
}

func newWaitService(t *testing.T, namespace, id string, process Process) *Service {
	t.Helper()
	service := &Service{
		processes: &processManager{ls: make(map[string]Process)},
	}
	if err := service.processes.Add(path.Join(namespace, id), process); err != nil {
		t.Fatalf("register process: %v", err)
	}
	return service
}
