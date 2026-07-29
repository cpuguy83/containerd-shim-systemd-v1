package main

import (
	"fmt"
	"slices"

	"github.com/containerd/errdefs"
)

// procPhase is where a process is in its lifecycle. It is the shim's authority
// on what may happen to a process next, and it is deliberately coarser than the
// systemd SubState carried in pState: systemd describes a unit, while a phase
// describes the containerd process the shim runs inside that unit.
//
// The two are complementary, not redundant. The phase decides created-vs-running
// in everything the shim reports, and decides whether an operation is allowed at
// all. The observation supplies the terminal detail the phase does not model:
// the exit code and time, and the failed / dead / exited-init distinction
// toStatus maps for containerd.
//
// The phases are ordered, so comparing against phaseRunning asks whether the
// process has finished starting. The zero value is phaseCreated, which is where
// every process begins.
type procPhase uint8

const (
	// phaseCreated is a container `runc create` has set up and a unit exists
	// for, but whose workload has not been started. systemd already reports such
	// a unit as running, which is why the phase -- not the observation -- is what
	// decides created-vs-running.
	phaseCreated procPhase = iota
	// phaseStarting is claimed by Start before it does any work, so a second
	// start, or a start of a process that has already exited, is rejected before
	// runc is invoked rather than after.
	phaseStarting
	// phaseRunning is reached once Start has returned successfully.
	phaseRunning
	// phaseExited is terminal for the workload: a terminal observation has been
	// merged and nothing will run under this process again.
	phaseExited
	// phaseDeleted is terminal for the process: teardown finished and every
	// further transition is rejected, so a late unit event cannot resurrect it.
	phaseDeleted
)

func (p procPhase) String() string {
	switch p {
	case phaseCreated:
		return "created"
	case phaseStarting:
		return "starting"
	case phaseRunning:
		return "running"
	case phaseExited:
		return "exited"
	case phaseDeleted:
		return "deleted"
	default:
		return fmt.Sprintf("procPhase(%d)", uint8(p))
	}
}

// phaseTransitions enumerates every legal transition. Self-transitions are
// listed where they are legal rather than allowed implicitly, so this table is
// the whole truth about the lifecycle:
//
//   - created -> created, running -> running and exited -> exited absorb
//     repeated observations; exited -> exited also lets a coarse exit be refined
//     by a later, more precise one without counting as a second exit.
//   - starting is a claim rather than an observation, so it is the one phase
//     that cannot repeat: only one caller may hold a start attempt at a time,
//     and running -> starting and exited -> starting reject starting a process
//     that is already running or can never run again.
//   - starting -> created hands the claim back when an attempt fails, so
//     another can be made.
//   - every phase may be deleted, because teardown has to work on a process that
//     never started as well as one that did.
var phaseTransitions = map[procPhase][]procPhase{
	phaseCreated:  {phaseCreated, phaseStarting, phaseExited, phaseDeleted},
	phaseStarting: {phaseCreated, phaseRunning, phaseExited, phaseDeleted},
	phaseRunning:  {phaseRunning, phaseExited, phaseDeleted},
	phaseExited:   {phaseExited, phaseDeleted},
	phaseDeleted:  {phaseDeleted},
}

func (p procPhase) canBecome(next procPhase) bool {
	return slices.Contains(phaseTransitions[p], next)
}

// invalidTransitionError reports a rejected lifecycle transition. It unwraps to
// errdefs.ErrFailedPrecondition so a rejected API operation reaches containerd
// with the right gRPC status without any extra mapping.
type invalidTransitionError struct {
	from, to procPhase
}

func (e invalidTransitionError) Error() string {
	return fmt.Sprintf("cannot go from %s to %s", e.from, e.to)
}

func (e invalidTransitionError) Unwrap() error {
	return errdefs.ErrFailedPrecondition
}
