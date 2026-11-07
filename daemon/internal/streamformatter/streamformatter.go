// Package streamformatter provides helper functions to format a stream.
package streamformatter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/v2/daemon/internal/compat"
	"github.com/moby/moby/v2/daemon/internal/progress"
)

const streamNewline = "\r\n"

type jsonProgressFormatter struct{}

func appendNewline(source []byte) []byte {
	return append(source, []byte(streamNewline)...)
}

// FormatStatus formats the specified objects according to the specified format (and id).
func FormatStatus(id, format string, a ...any) []byte {
	str := fmt.Sprintf(format, a...)
	b, err := json.Marshal(&jsonstream.Message{ID: id, Status: str})
	if err != nil {
		return FormatError(err)
	}
	return appendNewline(b)
}

// FormatError formats the error as a JSON object
func FormatError(err error) []byte {
	jsonError, ok := errors.AsType[*jsonstream.Error](err)
	if !ok {
		jsonError = &jsonstream.Error{Message: err.Error()}
	}

	// The "error" field was deprecated in favor of "errorDetail" in
	// https://github.com/moby/moby/commit/3043c2641990d94298c6377b7ef14709263a4709.
	//
	// Continue to produce both fields because integrations such as docker-py
	// still depend on the deprecated field:
	// https://github.com/docker/docker-py/blob/7.2.0/docker/models/images.py#L301-L304
	//
	// FIXME(thaJeztah): remove use of the deprecated "error" field once docker-py has been updated.
	b, err := json.Marshal(compat.Wrap(
		&jsonstream.Message{Error: jsonError},
		compat.WithExtraFields(map[string]any{"error": jsonError.Error()}),
	))
	if err != nil {
		return []byte(`{"error":"failed to marshal error response","errorDetail":{"code":500,"message":"failed to marshal error response"}}` + streamNewline)
	}
	return appendNewline(b)
}

func (sf *jsonProgressFormatter) formatStatus(id, format string, a ...any) []byte {
	return FormatStatus(id, format, a...)
}

// formatProgress formats the progress information for a specified action.
func (sf *jsonProgressFormatter) formatProgress(id, action string, progress *jsonstream.Progress, aux any) []byte {
	if progress == nil {
		progress = &jsonstream.Progress{}
	}
	var auxJSON *json.RawMessage
	if aux != nil {
		auxJSONBytes, err := json.Marshal(aux)
		if err != nil {
			return nil
		}
		auxJSON = new(json.RawMessage)
		*auxJSON = auxJSONBytes
	}
	b, err := json.Marshal(&jsonstream.Message{
		Status:   action,
		Progress: progress,
		ID:       id,
		Aux:      auxJSON,
	})
	if err != nil {
		return nil
	}
	return appendNewline(b)
}

// NewJSONProgressOutput returns a progress.Output that formats output
// using JSON objects
func NewJSONProgressOutput(out io.Writer, newLines bool) progress.Output {
	return &progressOutput{sf: &jsonProgressFormatter{}, out: out, newLines: newLines}
}

type formatProgress interface {
	formatStatus(id, format string, a ...any) []byte
	formatProgress(id, action string, progress *jsonstream.Progress, aux any) []byte
}

type progressOutput struct {
	sf       formatProgress
	out      io.Writer
	newLines bool
	mu       sync.Mutex
}

// WriteProgress formats progress information from a ProgressReader.
func (out *progressOutput) WriteProgress(prog progress.Progress) error {
	var formatted []byte
	if prog.Message != "" {
		formatted = out.sf.formatStatus(prog.ID, "%s", prog.Message)
	} else {
		jsonProgress := jsonstream.Progress{
			Current:    prog.Current,
			Total:      prog.Total,
			Start:      prog.Start,
			HideCounts: prog.HideCounts,
			Units:      prog.Units,
		}
		formatted = out.sf.formatProgress(prog.ID, prog.Action, &jsonProgress, prog.Aux)
	}

	out.mu.Lock()
	defer out.mu.Unlock()
	_, err := out.out.Write(formatted)
	if err != nil {
		return err
	}

	if out.newLines && prog.LastUpdate {
		_, err = out.out.Write(out.sf.formatStatus("", ""))
		return err
	}

	return nil
}

// AuxFormatter is a streamFormatter that writes aux progress messages
type AuxFormatter struct {
	io.Writer
}

// Emit emits the given interface as an aux progress message
func (sf *AuxFormatter) Emit(id string, aux any) error {
	auxJSONBytes, err := json.Marshal(aux)
	if err != nil {
		return err
	}
	auxJSON := new(json.RawMessage)
	*auxJSON = auxJSONBytes
	msgJSON, err := json.Marshal(&jsonstream.Message{ID: id, Aux: auxJSON})
	if err != nil {
		return err
	}
	msgJSON = appendNewline(msgJSON)
	n, err := sf.Writer.Write(msgJSON)
	if n != len(msgJSON) {
		return io.ErrShortWrite
	}
	return err
}
