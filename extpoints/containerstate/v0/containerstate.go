// SPDX-FileCopyrightText: Copyright The Moby Authors
// SPDX-License-Identifier: Apache-2.0

//go:generate go tool mobyextgen

// Package containerstatev0 defines a daemon-scoped extension point for
// watching container state.
package containerstatev0

import (
	"context"

	"github.com/moby/extensions"
	"github.com/moby/extensions/stream"
)

// ContainerStateWatcher watches container state.
//
// Wait and Watch resolve the container by ID, name, or unique ID prefix,
// and return NotFound if there is none.
// They return Unavailable until daemon startup completes.
//
// Wait returns the container's state once its status matches the request,
// and blocks until then otherwise.
// It returns immediately if the status already matches.
//
// A Wait blocked on a container that is removed returns its final state
// with status "removed", even if the request does not match it; a later
// Wait returns NotFound.
// An unknown status, or a request that sets both Statuses and NotStatuses,
// returns InvalidArgument.
//
// Watch sends the container's state, then its state after each status
// change, in order.
// It returns nil after sending the container's removal, with status
// "removed".
// It returns Unavailable if the caller falls too far behind, instead of
// skipping changes.
type ContainerStateWatcher interface {
	// Wait returns the container's state once its status matches req.
	Wait(ctx context.Context, req *WaitRequest) (*WaitReply, error)

	// Watch sends the container's state, then its state after each status
	// change, until the container is removed.
	Watch(ctx context.Context, req *WatchRequest, events stream.Sender[WatchEvent]) error
}

var Point = extensions.DefineSinglePoint[ContainerStateWatcher]("org.mobyproject.extension.container.state.v0")

// Container statuses used in [WaitRequest.Statuses],
// [WaitRequest.NotStatuses], and [ContainerState.Status].
const (
	StatusCreated  = "created"
	StatusRunning  = "running"
	StatusPaused   = "paused"
	StatusRemoving = "removing"
	StatusExited   = "exited"
	StatusDead     = "dead"
	StatusRemoved  = "removed"
	// StatusRestarting is the status between restart-policy attempts.
	StatusRestarting = "restarting"
)

// WaitRequest identifies the container and the statuses to wait for.
// Set at most one of Statuses and NotStatuses.
// If neither is set, Wait returns the current state immediately.
type WaitRequest struct {
	ContainerID string `pb:"1"`
	// Statuses makes Wait return once the container's status is one of
	// these.
	Statuses []string `pb:"2"`
	// NotStatuses makes Wait return once the container's status is none of
	// these.
	NotStatuses []string `pb:"3"`
}

// WaitReply contains the container's state.
type WaitReply struct {
	// ChangedAt is when the daemon observed the current status, in Unix
	// nanoseconds, or zero if it observed no status change since it started.
	ChangedAt int64 `pb:"1"`

	// Previous is the container's state just before its status last
	// changed, or nil if the daemon observed no status change since it
	// started.
	// For a removed container, it is the state at removal, usually with
	// status [StatusRemoving], and keeps the exit error.
	Previous *ContainerState `pb:"3"`

	// Current is the container's current state.
	Current *ContainerState `pb:"2"`
}

// WatchRequest identifies the container to watch.
type WatchRequest struct {
	ContainerID string `pb:"1"`
}

// WatchEvent is the container's state when Watch starts or after a status
// change.
// Its fields mean the same as in [WaitReply].
type WatchEvent struct {
	ChangedAt int64           `pb:"1"`
	Current   *ContainerState `pb:"2"`
	Previous  *ContainerState `pb:"3"`
}

// ContainerState is a container's status and most recent exit.
type ContainerState struct {
	// Status is one of the Status constants, such as [StatusRunning].
	// A container is [StatusRestarting], not [StatusExited], between
	// restart-policy attempts.
	Status string `pb:"1"`

	// Exit is the container's most recent exit, or nil while it is running
	// or before it first exits.
	Exit *Exit `pb:"2"`
}

// Exit describes a container exit.
type Exit struct {
	// ExitCode is the container's recorded exit code.
	ExitCode int64 `pb:"1"`
	// Error is the container's recorded error message, if any.
	// It is empty in a removed container's current state, because removal
	// clears it.
	Error string `pb:"2"`
	// OOMKilled reports whether the container's process was killed because
	// it ran out of memory.
	OOMKilled bool `pb:"3"`
}
