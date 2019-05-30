/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package cio

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"sync"

	"github.com/containerd/containerd/defaults"
)

var bufPool = sync.Pool{
	New: func() interface{} {
		buffer := make([]byte, 32<<10)
		return &buffer
	},
}

// Config holds the IO configurations.
type Config struct {
	// Terminal is true if one has been allocated
	Terminal bool
	// Stdin path
	Stdin string
	// Stdout path
	Stdout string
	// Stderr path
	Stderr string
}

// IO holds the io information for a task or process
type IO interface {
	// Config returns the IO configuration.
	Config() Config
	// Cancel aborts all current io operations.
	Cancel()
	// Wait blocks until all io copy operations have completed.
	Wait()
	// Close cleans up all open io resources. Cancel() is always called before
	// Close()
	Close() error
}

// Creator creates new IO sets for a task
type Creator func(id string) (IO, error)

// Attach allows callers to reattach to running tasks
//
// There should only be one reader for a task's IO set
// because fifo's can only be read from one reader or the output
// will be sent only to the first reads
type Attach func(*FIFOSet) (IO, error)

// FIFOSet is a set of file paths to FIFOs for a task's standard IO streams
type FIFOSet struct {
	Config
	close func() error
}

// Close the FIFOSet
func (f *FIFOSet) Close() error {
	if f.close != nil {
		return f.close()
	}
	return nil
}

// NewFIFOSet returns a new FIFOSet from a Config and a close function
func NewFIFOSet(config Config, close func() error) *FIFOSet {
	return &FIFOSet{Config: config, close: close}
}

// Streams used to configure a Creator or Attach
type Streams struct {
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	Terminal bool
	FIFODir  string
}

// Opt customize options for creating a Creator or Attach
type Opt func(*Streams)

// WithStdio sets stream options to the standard input/output streams
func WithStdio(opt *Streams) {
	WithStreams(os.Stdin, os.Stdout, os.Stderr)(opt)
}

// WithTerminal sets the terminal option
func WithTerminal(opt *Streams) {
	opt.Terminal = true
}

// WithStreams sets the stream options to the specified Reader and Writers
func WithStreams(stdin io.Reader, stdout, stderr io.Writer) Opt {
	return func(opt *Streams) {
		opt.Stdin = stdin
		opt.Stdout = stdout
		opt.Stderr = stderr
	}
}

// WithFIFODir sets the fifo directory.
// e.g. "/run/containerd/fifo", "/run/users/1001/containerd/fifo"
func WithFIFODir(dir string) Opt {
	return func(opt *Streams) {
		opt.FIFODir = dir
	}
}

// NewCreator returns an IO creator from the options
func NewCreator(opts ...Opt) Creator {
	streams := &Streams{}
	for _, opt := range opts {
		opt(streams)
	}
	if streams.FIFODir == "" {
		streams.FIFODir = defaults.DefaultFIFODir
	}
	return func(id string) (IO, error) {
		fifos, err := NewFIFOSetInDir(streams.FIFODir, id, streams.Terminal)
		if err != nil {
			return nil, err
		}
		if streams.Stdin == nil {
			fifos.Stdin = ""
		}
		if streams.Stdout == nil {
			fifos.Stdout = ""
		}
		if streams.Stderr == nil {
			fifos.Stderr = ""
		}
		return copyIO(fifos, streams)
	}
}

// NewAttach attaches the existing io for a task to the provided io.Reader/Writers
func NewAttach(opts ...Opt) Attach {
	streams := &Streams{}
	for _, opt := range opts {
		opt(streams)
	}
	return func(fifos *FIFOSet) (IO, error) {
		if fifos == nil {
			return nil, fmt.Errorf("cannot attach, missing fifos")
		}
		return copyIO(fifos, streams)
	}
}

// NullIO redirects the container's IO into /dev/null
func NullIO(_ string) (IO, error) {
	return &cio{}, nil
}

// cio is a basic container IO implementation.
type cio struct {
	config  Config
	wg      *sync.WaitGroup
	closers []io.Closer
	cancel  context.CancelFunc
}

func (c *cio) Config() Config {
	return c.config
}

func (c *cio) Wait() {
	if c.wg != nil {
		c.wg.Wait()
	}
}

func (c *cio) Close() error {
	var lastErr error
	for _, closer := range c.closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func (c *cio) Cancel() {
	if c.cancel != nil {
		c.cancel()
	}
}

type pipes struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
}

// DirectIO allows task IO to be handled externally by the caller
type DirectIO struct {
	pipes
	cio
}

var (
	_ IO = &DirectIO{}
	_ IO = &logURI{}
)

// LogURI provides the raw logging URI
func LogURI(uri *url.URL) Creator {
	return func(_ string) (IO, error) {
		return &logURI{
			config: Config{
				Stdout: uri.String(),
				Stderr: uri.String(),
			},
		}, nil
	}
}

// BinaryIO forwards container STDOUT|STDERR directly to a logging binary
func BinaryIO(binary string, args map[string]string) Creator {
	return func(_ string) (IO, error) {
		uri := &url.URL{
			Scheme: "binary",
			Host:   binary,
		}
		for k, v := range args {
			uri.Query().Set(k, v)
		}
		return &logURI{
			config: Config{
				Stdout: uri.String(),
				Stderr: uri.String(),
			},
		}, nil
	}
}

// LogFile creates a file on disk that logs the task's STDOUT,STDERR.
// If the log file already exists, the logs will be appended to the file.
func LogFile(path string) Creator {
	return func(_ string) (IO, error) {
		uri := &url.URL{
			Scheme: "file",
			Host:   path,
		}
		return &logURI{
			config: Config{
				Stdout: uri.String(),
				Stderr: uri.String(),
			},
		}, nil
	}
}

type logURI struct {
	config Config
}

func (l *logURI) Config() Config {
	return l.config
}

func (l *logURI) Cancel() {

}

func (l *logURI) Wait() {

}

func (l *logURI) Close() error {
	return nil
}

// Load the io for a container but do not attach
//
// Allows io to be loaded on the task for deletion without
// starting copy routines
func Load(set *FIFOSet) (IO, error) {
	return &cio{
		config:  set.Config,
		closers: []io.Closer{set},
	}, nil
}

func (p *pipes) closers() []io.Closer {
	return []io.Closer{p.Stdin, p.Stdout, p.Stderr}
}
