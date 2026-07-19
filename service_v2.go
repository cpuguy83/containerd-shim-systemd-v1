package main

import (
	"context"

	taskv2 "github.com/containerd/containerd/api/runtime/task/v2"
	taskv3 "github.com/containerd/containerd/api/runtime/task/v3"
	"google.golang.org/protobuf/types/known/emptypb"
)

type taskV2Adapter struct {
	service taskv3.TTRPCTaskService
}

var _ taskv2.TTRPCTaskService = (*taskV2Adapter)(nil)

func (a *taskV2Adapter) State(ctx context.Context, request *taskv2.StateRequest) (*taskv2.StateResponse, error) {
	response, err := a.service.State(ctx, &taskv3.StateRequest{
		ID:     request.GetID(),
		ExecID: request.GetExecID(),
	})
	return &taskv2.StateResponse{
		ID:         response.GetID(),
		Bundle:     response.GetBundle(),
		Pid:        response.GetPid(),
		Status:     response.GetStatus(),
		Stdin:      response.GetStdin(),
		Stdout:     response.GetStdout(),
		Stderr:     response.GetStderr(),
		Terminal:   response.GetTerminal(),
		ExitStatus: response.GetExitStatus(),
		ExitedAt:   response.GetExitedAt(),
		ExecID:     response.GetExecID(),
	}, err
}

func (a *taskV2Adapter) Create(ctx context.Context, request *taskv2.CreateTaskRequest) (*taskv2.CreateTaskResponse, error) {
	response, err := a.service.Create(ctx, &taskv3.CreateTaskRequest{
		ID:               request.GetID(),
		Bundle:           request.GetBundle(),
		Rootfs:           request.GetRootfs(),
		Terminal:         request.GetTerminal(),
		Stdin:            request.GetStdin(),
		Stdout:           request.GetStdout(),
		Stderr:           request.GetStderr(),
		Checkpoint:       request.GetCheckpoint(),
		ParentCheckpoint: request.GetParentCheckpoint(),
		Options:          request.GetOptions(),
	})
	return &taskv2.CreateTaskResponse{Pid: response.GetPid()}, err
}

func (a *taskV2Adapter) Start(ctx context.Context, request *taskv2.StartRequest) (*taskv2.StartResponse, error) {
	response, err := a.service.Start(ctx, &taskv3.StartRequest{
		ID:     request.GetID(),
		ExecID: request.GetExecID(),
	})
	return &taskv2.StartResponse{Pid: response.GetPid()}, err
}

func (a *taskV2Adapter) Delete(ctx context.Context, request *taskv2.DeleteRequest) (*taskv2.DeleteResponse, error) {
	response, err := a.service.Delete(ctx, &taskv3.DeleteRequest{
		ID:     request.GetID(),
		ExecID: request.GetExecID(),
	})
	return &taskv2.DeleteResponse{
		Pid:        response.GetPid(),
		ExitStatus: response.GetExitStatus(),
		ExitedAt:   response.GetExitedAt(),
	}, err
}

func (a *taskV2Adapter) Pids(ctx context.Context, request *taskv2.PidsRequest) (*taskv2.PidsResponse, error) {
	response, err := a.service.Pids(ctx, &taskv3.PidsRequest{ID: request.GetID()})
	return &taskv2.PidsResponse{Processes: response.GetProcesses()}, err
}

func (a *taskV2Adapter) Pause(ctx context.Context, request *taskv2.PauseRequest) (*emptypb.Empty, error) {
	return a.service.Pause(ctx, &taskv3.PauseRequest{ID: request.GetID()})
}

func (a *taskV2Adapter) Resume(ctx context.Context, request *taskv2.ResumeRequest) (*emptypb.Empty, error) {
	return a.service.Resume(ctx, &taskv3.ResumeRequest{ID: request.GetID()})
}

func (a *taskV2Adapter) Checkpoint(ctx context.Context, request *taskv2.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return a.service.Checkpoint(ctx, &taskv3.CheckpointTaskRequest{
		ID:      request.GetID(),
		Path:    request.GetPath(),
		Options: request.GetOptions(),
	})
}

func (a *taskV2Adapter) Kill(ctx context.Context, request *taskv2.KillRequest) (*emptypb.Empty, error) {
	return a.service.Kill(ctx, &taskv3.KillRequest{
		ID:     request.GetID(),
		ExecID: request.GetExecID(),
		Signal: request.GetSignal(),
		All:    request.GetAll(),
	})
}

func (a *taskV2Adapter) Exec(ctx context.Context, request *taskv2.ExecProcessRequest) (*emptypb.Empty, error) {
	return a.service.Exec(ctx, &taskv3.ExecProcessRequest{
		ID:       request.GetID(),
		ExecID:   request.GetExecID(),
		Terminal: request.GetTerminal(),
		Stdin:    request.GetStdin(),
		Stdout:   request.GetStdout(),
		Stderr:   request.GetStderr(),
		Spec:     request.GetSpec(),
	})
}

func (a *taskV2Adapter) ResizePty(ctx context.Context, request *taskv2.ResizePtyRequest) (*emptypb.Empty, error) {
	return a.service.ResizePty(ctx, &taskv3.ResizePtyRequest{
		ID:     request.GetID(),
		ExecID: request.GetExecID(),
		Width:  request.GetWidth(),
		Height: request.GetHeight(),
	})
}

func (a *taskV2Adapter) CloseIO(ctx context.Context, request *taskv2.CloseIORequest) (*emptypb.Empty, error) {
	return a.service.CloseIO(ctx, &taskv3.CloseIORequest{
		ID:     request.GetID(),
		ExecID: request.GetExecID(),
		Stdin:  request.GetStdin(),
	})
}

func (a *taskV2Adapter) Update(ctx context.Context, request *taskv2.UpdateTaskRequest) (*emptypb.Empty, error) {
	return a.service.Update(ctx, &taskv3.UpdateTaskRequest{
		ID:          request.GetID(),
		Resources:   request.GetResources(),
		Annotations: request.GetAnnotations(),
	})
}

func (a *taskV2Adapter) Wait(ctx context.Context, request *taskv2.WaitRequest) (*taskv2.WaitResponse, error) {
	response, err := a.service.Wait(ctx, &taskv3.WaitRequest{
		ID:     request.GetID(),
		ExecID: request.GetExecID(),
	})
	return &taskv2.WaitResponse{
		ExitStatus: response.GetExitStatus(),
		ExitedAt:   response.GetExitedAt(),
	}, err
}

func (a *taskV2Adapter) Stats(ctx context.Context, request *taskv2.StatsRequest) (*taskv2.StatsResponse, error) {
	response, err := a.service.Stats(ctx, &taskv3.StatsRequest{ID: request.GetID()})
	return &taskv2.StatsResponse{Stats: response.GetStats()}, err
}

func (a *taskV2Adapter) Connect(ctx context.Context, request *taskv2.ConnectRequest) (*taskv2.ConnectResponse, error) {
	response, err := a.service.Connect(ctx, &taskv3.ConnectRequest{ID: request.GetID()})
	return &taskv2.ConnectResponse{
		ShimPid: response.GetShimPid(),
		TaskPid: response.GetTaskPid(),
		Version: response.GetVersion(),
	}, err
}

func (a *taskV2Adapter) Shutdown(ctx context.Context, request *taskv2.ShutdownRequest) (*emptypb.Empty, error) {
	return a.service.Shutdown(ctx, &taskv3.ShutdownRequest{
		ID:  request.GetID(),
		Now: request.GetNow(),
	})
}
