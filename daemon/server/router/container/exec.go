package container

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/containerd/log"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/internal/stdcopymux"
	"github.com/moby/moby/v2/daemon/internal/versions"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/daemon/server/httputils"
	"github.com/moby/moby/v2/errdefs"
	"github.com/pkg/errors"
)

func (c *containerRouter) getExecByID(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	eConfig, err := c.backend.ContainerExecInspect(vars["id"])
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, eConfig)
}

type execCommandError struct{}

func (execCommandError) Error() string {
	return "No exec command specified"
}

func (execCommandError) InvalidParameter() {}

func (c *containerRouter) postContainerExecCreate(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	execConfig := &container.ExecCreateRequest{}
	if err := httputils.ReadJSON(r, execConfig); err != nil {
		return err
	}

	if len(execConfig.Cmd) == 0 {
		return execCommandError{}
	}

	// Register an instance of Exec in container.
	id, err := c.backend.ContainerExecCreate(vars["name"], execConfig)
	if err != nil {
		log.G(ctx).Errorf("Error setting up exec command in container %s: %v", vars["name"], err)
		return err
	}

	return httputils.WriteJSON(w, http.StatusCreated, &container.ExecCreateResponse{
		ID: id,
	})
}

// TODO(vishh): Refactor the code to avoid having to specify stream config as part of both create and start.
func (c *containerRouter) postContainerExecStart(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var (
		execName                  = vars["name"]
		stdin, inStream           io.ReadCloser
		stdout, stderr, outStream io.Writer
	)

	options := &container.ExecStartRequest{}
	if err := httputils.ReadJSON(r, options); err != nil {
		return err
	}

	if exists, err := c.backend.ExecExists(execName); !exists {
		return err
	}

	if !options.Tty {
		// No console without tty
		options.ConsoleSize = nil
	}

	if !options.Detach {
		var err error
		// Setting up the streaming http interface.
		inStream, outStream, err = httputils.HijackConnection(w)
		if err != nil {
			return err
		}
		defer httputils.CloseStreams(inStream, outStream)

		if _, ok := r.Header["Upgrade"]; ok {
			contentType := types.MediaTypeRawStream
			if !options.Tty && versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.42") {
				contentType = types.MediaTypeMultiplexedStream
			}
			_, _ = fmt.Fprint(outStream, "HTTP/1.1 101 UPGRADED\r\nContent-Type: "+contentType+"\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n")
		} else {
			_, _ = fmt.Fprint(outStream, "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n")
		}

		// copy headers that were removed as part of hijack
		if err := w.Header().WriteSubset(outStream, nil); err != nil {
			return err
		}
		_, _ = fmt.Fprint(outStream, "\r\n")

		stdin = inStream
		if options.Tty {
			stdout = outStream
		} else {
			stderr = stdcopymux.NewStdWriter(outStream, stdcopy.Stderr)
			stdout = stdcopymux.NewStdWriter(outStream, stdcopy.Stdout)
		}
	}

	// Now run the user process in container.
	//
	// TODO: Maybe we should we pass ctx here if we're not detaching?
	err := c.backend.ContainerExecStart(context.Background(), execName, backend.ExecStartConfig{
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		ConsoleSize: options.ConsoleSize,
	})
	if err != nil {
		if options.Detach {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "%v\r\n", err)
		log.G(ctx).Errorf("Error running exec %s in container: %v", execName, err)
	}
	return nil
}

func (c *containerRouter) postContainerExecResize(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	height, err := httputils.Uint32Value(r, "h")
	if err != nil {
		return errdefs.InvalidParameter(errors.Wrapf(err, "invalid resize height %q", r.Form.Get("h")))
	}
	width, err := httputils.Uint32Value(r, "w")
	if err != nil {
		return errdefs.InvalidParameter(errors.Wrapf(err, "invalid resize width %q", r.Form.Get("w")))
	}

	return c.backend.ContainerExecResize(ctx, vars["name"], height, width)
}
