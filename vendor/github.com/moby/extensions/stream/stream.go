// SPDX-FileCopyrightText: Copyright The Moby Authors
// SPDX-License-Identifier: Apache-2.0

// Package stream defines typed, method-scoped streaming contracts.
//
// The owning method controls stream lifetime and its context controls
// cancellation.
package stream

import (
	"errors"
	"io"
	"iter"
)

// Sender sends messages to the other side of a method-scoped stream.
//
// Calls to one Sender must be serialized. The caller must not mutate the
// message, or data reachable from it, until the owning method returns.
// Send must treat that data as read-only and may retain it until then.
// A nil message is invalid.
//
// A successful Send does not report final method success or durable storage.
// Implementations that block must cooperate with the context of the method
// that owns the stream.
type Sender[T any] interface {
	Send(*T) error
}

// Receiver receives messages from the other side of a method-scoped stream.
//
// Calls to one Receiver must be serialized. Recv returns [io.EOF] only for
// normal input completion. A successful call must return a non-nil message.
// Messages returned by Recv are read-only to the caller and must not be used
// after the method that owns the stream returns.
//
// Implementations that block must cooperate with the context of the method
// that owns the stream.
// The method cannot interrupt a blocked Recv in any other way.
// If the other side finishes early, for example with an error, the method
// sees that result only after Recv returns.
type Receiver[T any] interface {
	Recv() (*T, error)
}

var errNilMessage = errors.New("stream: nil message")

// All adapts a Receiver to an iterator.
//
// Each successful message is yielded with a nil error. All stops at [io.EOF],
// including an error wrapping [io.EOF], without yielding the EOF. For any
// other receive error, All yields a nil message and that error once, then
// stops.
//
// A receiver that returns a nil message with a nil error is invalid. All
// reports that condition as an ordinary error and stops.
//
// If the yield callback returns false, All makes no more receive calls.
// Stopping the iterator does not cancel a receive that is already in progress;
// the caller must end or cancel the owning method. Neither All nor the stream
// interfaces can forcibly interrupt an interface callback.
func All[T any](receiver Receiver[T]) iter.Seq2[*T, error] {
	return func(yield func(*T, error) bool) {
		for {
			message, err := receiver.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return
				}
				yield(nil, err)
				return
			}
			if message == nil {
				yield(nil, errNilMessage)
				return
			}
			if !yield(message, nil) {
				return
			}
		}
	}
}

// Copy sends each message from source through sender.
//
// Copy stops source and returns the first source error, nil-message error, or
// sender error.
// Use [All] to translate a receiver's normal EOF into iterator completion.
//
// Source must honor a false return from its yield callback. Copy can stop a
// source through that callback, but cannot forcibly interrupt source code that
// is already executing.
func Copy[T any](sender Sender[T], source iter.Seq2[*T, error]) error {
	for message, err := range source {
		if err != nil {
			return err
		}
		if message == nil {
			return errNilMessage
		}
		if err := sender.Send(message); err != nil {
			return err
		}
	}
	return nil
}
