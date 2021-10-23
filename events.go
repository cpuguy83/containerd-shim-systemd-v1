package main

import (
	"context"
	"io"

	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/runtime"
	"github.com/sirupsen/logrus"
)

func (s *Service) Forward(ctx context.Context, publisher events.Publisher) {
	for e := range s.events {
		ctx := namespaces.WithNamespace(ctx, e.ns)
		err := publisher.Publish(ctx, GetTopic(e.e), e.e)
		if err != nil {
			logrus.WithError(err).Error("post event")
		}
	}
	if closer, ok := publisher.(io.Closer); ok {
		closer.Close()
	}
	close(s.waitEvents)
}

type eventEnvelope struct {
	ns string
	e  interface{}
}

func (s *Service) send(ns string, e interface{}) {
	s.events <- eventEnvelope{ns, e}
}

// GetTopic converts an event from an interface type to the specific
// event topic id
func GetTopic(e interface{}) string {
	switch e.(type) {
	case *eventsapi.TaskCreate:
		return runtime.TaskCreateEventTopic
	case *eventsapi.TaskStart:
		return runtime.TaskStartEventTopic
	case *eventsapi.TaskOOM:
		return runtime.TaskOOMEventTopic
	case *eventsapi.TaskExit:
		return runtime.TaskExitEventTopic
	case *eventsapi.TaskDelete:
		return runtime.TaskDeleteEventTopic
	case *eventsapi.TaskExecAdded:
		return runtime.TaskExecAddedEventTopic
	case *eventsapi.TaskExecStarted:
		return runtime.TaskExecStartedEventTopic
	case *eventsapi.TaskPaused:
		return runtime.TaskPausedEventTopic
	case *eventsapi.TaskResumed:
		return runtime.TaskResumedEventTopic
	case *eventsapi.TaskCheckpointed:
		return runtime.TaskCheckpointedEventTopic
	default:
		logrus.Warnf("no topic for type %#v", e)
	}
	return runtime.TaskUnknownTopic
}
