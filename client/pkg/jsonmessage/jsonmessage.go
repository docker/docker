package jsonmessage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
	"time"

	"github.com/docker/go-units"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/term"
)

var timeNow = time.Now // For overriding in tests.

// RFC3339NanoFixed is time.RFC3339Nano with nanoseconds padded using zeros to
// ensure the formatted time is always the same number of characters.
const RFC3339NanoFixed = "2006-01-02T15:04:05.000000000Z07:00"

// DisplayOpt configures behavior for [DisplayStream] and [DisplayMessages].
// Options are applied in order; if an option returns an error, processing
// stops and the error is returned to the caller.
type DisplayOpt func(*displayOpts) error

type displayOpts struct {
	auxCallback    func(jsonstream.Message)
	messagePrinter func(jsonstream.Message, io.Writer) error
}

// WithAuxCallback registers a callback that is invoked for auxiliary
// jsonstream messages as they are processed. The callback is optional;
// if not provided, auxiliary messages are ignored.
func WithAuxCallback(fn func(jsonstream.Message)) DisplayOpt {
	return func(opts *displayOpts) error {
		opts.auxCallback = fn
		return nil
	}
}

// WithMessagePrinter sets a custom printer for non-auxiliary messages.
// If set, [DisplayStream] and [DisplayMessages] pass each message to fn instead
// of using the default terminal-aware presentation. The caller is responsible
// for handling errors reported through [jsonstream.Message.Error] and any
// terminal handling, including progress rendering and terminal-width detection.
func WithMessagePrinter(fn func(jsonstream.Message, io.Writer) error) DisplayOpt {
	return func(opts *displayOpts) error {
		opts.messagePrinter = fn
		return nil
	}
}

func RenderTUIProgress(p jsonstream.Progress, width uint16) string {
	var (
		pbBox      string
		numbersBox string
	)
	if p.Current <= 0 && p.Total <= 0 {
		return ""
	}
	if p.Total <= 0 {
		switch p.Units {
		case "":
			return fmt.Sprintf("%8v", units.HumanSize(float64(p.Current)))
		default:
			return fmt.Sprintf("%d %s", p.Current, p.Units)
		}
	}

	// percentage can't be negative gh#7136
	percentage := min(max(int(float64(p.Current)/float64(p.Total)*100)/2, 0), 50)
	if width > 110 {
		// this number can't be negative gh#7136
		numSpaces := max(50-percentage, 0)
		pbBox = fmt.Sprintf("[%s>%s] ", strings.Repeat("=", percentage), strings.Repeat(" ", numSpaces))
	}

	switch {
	case p.HideCounts:
	case p.Units == "": // no units, use bytes
		current := units.HumanSize(float64(p.Current))
		total := units.HumanSize(float64(p.Total))

		numbersBox = fmt.Sprintf("%8v/%v", current, total)

		if p.Current > p.Total {
			// remove total display if the reported current is wonky.
			numbersBox = fmt.Sprintf("%8v", current)
		}
	default:
		numbersBox = fmt.Sprintf("%d/%d %s", p.Current, p.Total, p.Units)

		if p.Current > p.Total {
			// remove total display if the reported current is wonky.
			numbersBox = fmt.Sprintf("%d %s", p.Current, p.Units)
		}
	}

	// Show approximation of remaining time if there's enough width.
	var timeLeftBox string
	if width > 50 {
		if p.Current > 0 && p.Start > 0 && percentage < 50 {
			fromStart := timeNow().UTC().Sub(time.Unix(p.Start, 0))
			perEntry := fromStart / time.Duration(p.Current)
			left := time.Duration(p.Total-p.Current) * perEntry
			timeLeftBox = " " + left.Round(time.Second).String()
		}
	}
	return pbBox + numbersBox + timeLeftBox
}

// We can probably use [aec.EmptyBuilder] for managing the output, but
// currently we're doing it all manually, so defining some consts for
// the basics we use.
//
// [aec.EmptyBuilder]: https://pkg.go.dev/github.com/morikuni/aec#EmptyBuilder
const (
	ansiEraseLine     = "\x1b[2K"  // Erase entire line
	ansiCursorUpFmt   = "\x1b[%dA" // Move cursor up N lines
	ansiCursorDownFmt = "\x1b[%dB" // Move cursor down N lines
)

func clearLine(out io.Writer) {
	_, _ = out.Write([]byte(ansiEraseLine))
}

func cursorUp(out io.Writer, l uint) {
	if l == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, ansiCursorUpFmt, l)
}

func cursorDown(out io.Writer, l uint) {
	if l == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, ansiCursorDownFmt, l)
}

// Display prints the JSONMessage to out. If isTerminal is true, it erases
// the entire current line when displaying the progressbar. It returns an
// error if [jsonstream.Message.Error] is non-nil.
func Display(jm jsonstream.Message, out io.Writer, isTerminal bool, width uint16) error {
	if jm.Error != nil {
		return jm.Error
	}
	var endl string
	if isTerminal && jm.Stream == "" && jm.Progress != nil {
		clearLine(out)
		endl = "\r"
		_, _ = fmt.Fprint(out, endl)
	} else if jm.Progress != nil && (jm.Progress.Current > 0 || jm.Progress.Total > 0) { // disable progressbar in non-terminal
		return nil
	}
	if jm.ID != "" {
		_, _ = fmt.Fprintf(out, "%s: ", jm.ID)
	}
	if jm.Progress != nil && isTerminal {
		if width == 0 {
			width = 200
		}
		_, _ = fmt.Fprintf(out, "%s %s%s", jm.Status, RenderTUIProgress(*jm.Progress, width), endl)
	} else if jm.Stream != "" {
		_, _ = fmt.Fprintf(out, "%s%s", jm.Stream, endl)
	} else {
		_, _ = fmt.Fprintf(out, "%s%s\n", jm.Status, endl)
	}
	return nil
}

type JSONMessagesStream = iter.Seq2[jsonstream.Message, error]

// DisplayJSONMessagesStream is like [DisplayStream], but allows the caller to
// explicitly provide the terminal file descriptor and whether out is a terminal.
func DisplayJSONMessagesStream(in io.Reader, out io.Writer, terminalFd uintptr, isTerminal bool, auxCallback func(jsonstream.Message)) error {
	var opts []DisplayOpt
	if auxCallback != nil {
		opts = append(opts, WithAuxCallback(auxCallback))
	}
	return displayJSONMessagesStream(in, out, terminalFd, isTerminal, opts...)
}

// DisplayStream reads a JSON message stream from in, and writes each
// [jsonstream.Message] to out. See [DisplayMessages] for details.
func DisplayStream(in io.Reader, out io.Writer, opts ...DisplayOpt) error {
	terminalFd, isTerminal := term.GetFdInfo(out)
	return displayJSONMessagesStream(in, out, terminalFd, isTerminal, opts...)
}

func displayJSONMessagesStream(in io.Reader, out io.Writer, terminalFd uintptr, isTerminal bool, opts ...DisplayOpt) error {
	dec := json.NewDecoder(in)
	f := func(yield func(jsonstream.Message, error) bool) {
		for {
			var jm jsonstream.Message
			err := dec.Decode(&jm)
			if errors.Is(err, io.EOF) {
				break
			}
			if !yield(jm, err) {
				return
			}
		}
	}

	return displayJSONMessages(f, out, terminalFd, isTerminal, opts...)
}

// DisplayJSONMessages is like [DisplayMessages], but allows the caller to
// explicitly provide the terminal file descriptor and whether out is a terminal.
func DisplayJSONMessages(messages iter.Seq2[jsonstream.Message, error], out io.Writer, terminalFd uintptr, isTerminal bool, auxCallback func(jsonstream.Message)) error {
	var opts []DisplayOpt
	if auxCallback != nil {
		opts = append(opts, WithAuxCallback(auxCallback))
	}
	return displayJSONMessages(messages, out, terminalFd, isTerminal, opts...)
}

// DisplayMessages writes each [jsonstream.Message] from stream to out.
// It returns an error if an invalid [jsonstream.Message] is received, or if
// a message contains a non-zero [jsonstream.Message.Error].
//
// Presentation of the message depends on whether out is a terminal, and on the
// terminal width. Progress bars ([jsonstream.Progress]) are suppressed on
// narrower terminals (< 110 characters). If out is a terminal, it prints a
// newline ("\n") at the end of each line and moves the cursor while displaying.
//
// auxCallback allows handling the [jsonstream.Message.Aux] field. It is called
// if a message contains an Aux field, in which case DisplayMessages does not
// present the message.
func DisplayMessages(messages iter.Seq2[jsonstream.Message, error], out io.Writer, opts ...DisplayOpt) error {
	terminalFd, isTerminal := term.GetFdInfo(out)
	return displayJSONMessages(messages, out, terminalFd, isTerminal, opts...)
}

func displayJSONMessages(messages iter.Seq2[jsonstream.Message, error], out io.Writer, terminalFd uintptr, isTerminal bool, opts ...DisplayOpt) error {
	var cfg displayOpts
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(&cfg); err != nil {
			return err
		}
	}
	auxCallback := cfg.auxCallback
	messagePrinter := cfg.messagePrinter

	var (
		ids   map[string]uint
		width uint16 = 200
	)
	if messagePrinter == nil {
		ids = make(map[string]uint)
		if isTerminal {
			ws, err := term.GetWinsize(terminalFd)
			if err == nil {
				width = ws.Width
			}
		}
	}

	for jm, err := range messages {
		var diff uint
		if err != nil {
			return err
		}

		if jm.Aux != nil {
			if auxCallback != nil {
				auxCallback(jm)
			}
			continue
		}

		if messagePrinter != nil {
			if err := messagePrinter(jm, out); err != nil {
				return err
			}
			continue
		}

		if jm.ID != "" && jm.Progress != nil {
			line, ok := ids[jm.ID]
			if !ok {
				// NOTE: This approach of using len(id) to
				// figure out the number of lines of history
				// only works as long as we clear the history
				// when we output something that's not
				// accounted for in the map, such as a line
				// with no ID.
				line = uint(len(ids))
				ids[jm.ID] = line
				if isTerminal {
					_, _ = fmt.Fprintf(out, "\n")
				}
			}
			diff = uint(len(ids)) - line
			if isTerminal {
				cursorUp(out, diff)
			}
		} else {
			// When outputting something that isn't progress
			// output, clear the history of previous lines. We
			// don't want progress entries from some previous
			// operation to be updated (for example, pull -a
			// with multiple tags).
			ids = make(map[string]uint)
		}
		err := Display(jm, out, isTerminal, width)
		if jm.ID != "" && isTerminal {
			cursorDown(out, diff)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
