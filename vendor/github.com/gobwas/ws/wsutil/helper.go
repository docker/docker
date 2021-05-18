package wsutil

import (
	"bytes"
	"io"
	"io/ioutil"

	"github.com/gobwas/ws"
)

// Message represents a message from peer, that could be presented in one or
// more frames. That is, it contains payload of all message fragments and
// operation code of initial frame for this message.
type Message struct {
	OpCode  ws.OpCode
	Payload []byte
}

// ReadMessage is a helper function that reads next message from r. It appends
// received message(s) to the third argument and returns the result of it and
// an error if some failure happened. That is, it probably could receive more
// than one message when peer sending fragmented message in multiple frames and
// want to send some control frame between fragments. Then returned slice will
// contain those control frames at first, and then result of gluing fragments.
//
// TODO(gobwas): add DefaultReader with buffer size options.
func ReadMessage(r io.Reader, s ws.State, m []Message) ([]Message, error) {
	rd := Reader{
		Source:    r,
		State:     s,
		CheckUTF8: true,
		OnIntermediate: func(hdr ws.Header, src io.Reader) error {
			bts, err := ioutil.ReadAll(src)
			if err != nil {
				return err
			}
			m = append(m, Message{hdr.OpCode, bts})
			return nil
		},
	}
	h, err := rd.NextFrame()
	if err != nil {
		return m, err
	}
	var p []byte
	if h.Fin {
		// No more frames will be read. Use fixed sized buffer to read payload.
		p = make([]byte, h.Length)
		// It is not possible to receive io.EOF here because Reader does not
		// return EOF if frame payload was successfully fetched.
		// Thus we consistent here with io.Reader behavior.
		_, err = io.ReadFull(&rd, p)
	} else {
		// Frame is fragmented, thus use ioutil.ReadAll behavior.
		var buf bytes.Buffer
		_, err = buf.ReadFrom(&rd)
		p = buf.Bytes()
	}
	if err != nil {
		return m, err
	}
	return append(m, Message{h.OpCode, p}), nil
}

// ReadClientMessage reads next message from r, considering that caller
// represents server side.
// It is a shortcut for ReadMessage(r, ws.StateServerSide, m)
func ReadClientMessage(r io.Reader, m []Message) ([]Message, error) {
	return ReadMessage(r, ws.StateServerSide, m)
}

// ReadServerMessage reads next message from r, considering that caller
// represents client side.
// It is a shortcut for ReadMessage(r, ws.StateClientSide, m)
func ReadServerMessage(r io.Reader, m []Message) ([]Message, error) {
	return ReadMessage(r, ws.StateClientSide, m)
}

// ReadData is a helper function that reads next data (non-control) message
// from rw.
// It takes care on handling all control frames. It will write response on
// control frames to the write part of rw. It blocks until some data frame
// will be received.
//
// Note this may handle and write control frames into the writer part of a
// given io.ReadWriter.
func ReadData(rw io.ReadWriter, s ws.State) ([]byte, ws.OpCode, error) {
	return readData(rw, s, ws.OpText|ws.OpBinary)
}

// ReadClientData reads next data message from rw, considering that caller
// represents server side. It is a shortcut for ReadData(rw, ws.StateServerSide).
//
// Note this may handle and write control frames into the writer part of a
// given io.ReadWriter.
func ReadClientData(rw io.ReadWriter) ([]byte, ws.OpCode, error) {
	return ReadData(rw, ws.StateServerSide)
}

// ReadClientText reads next text message from rw, considering that caller
// represents server side. It is a shortcut for ReadData(rw, ws.StateServerSide).
// It discards received binary messages.
//
// Note this may handle and write control frames into the writer part of a
// given io.ReadWriter.
func ReadClientText(rw io.ReadWriter) ([]byte, error) {
	p, _, err := readData(rw, ws.StateServerSide, ws.OpText)
	return p, err
}

// ReadClientBinary reads next binary message from rw, considering that caller
// represents server side. It is a shortcut for ReadData(rw, ws.StateServerSide).
// It discards received text messages.
//
// Note this may handle and write control frames into the writer part of a given
//  io.ReadWriter.
func ReadClientBinary(rw io.ReadWriter) ([]byte, error) {
	p, _, err := readData(rw, ws.StateServerSide, ws.OpBinary)
	return p, err
}

// ReadServerData reads next data message from rw, considering that caller
// represents client side. It is a shortcut for ReadData(rw, ws.StateClientSide).
//
// Note this may handle and write control frames into the writer part of a
// given io.ReadWriter.
func ReadServerData(rw io.ReadWriter) ([]byte, ws.OpCode, error) {
	return ReadData(rw, ws.StateClientSide)
}

// ReadServerText reads next text message from rw, considering that caller
// represents client side. It is a shortcut for ReadData(rw, ws.StateClientSide).
// It discards received binary messages.
//
// Note this may handle and write control frames into the writer part of a given
//  io.ReadWriter.
func ReadServerText(rw io.ReadWriter) ([]byte, error) {
	p, _, err := readData(rw, ws.StateClientSide, ws.OpText)
	return p, err
}

// ReadServerBinary reads next binary message from rw, considering that caller
// represents client side. It is a shortcut for ReadData(rw, ws.StateClientSide).
// It discards received text messages.
//
// Note this may handle and write control frames into the writer part of a
// given io.ReadWriter.
func ReadServerBinary(rw io.ReadWriter) ([]byte, error) {
	p, _, err := readData(rw, ws.StateClientSide, ws.OpBinary)
	return p, err
}

// WriteMessage is a helper function that writes message to the w. It
// constructs single frame with given operation code and payload.
// It uses given state to prepare side-dependent things, like cipher
// payload bytes from client to server. It will not mutate p bytes if
// cipher must be made.
//
// If you want to write message in fragmented frames, use Writer instead.
func WriteMessage(w io.Writer, s ws.State, op ws.OpCode, p []byte) error {
	return writeFrame(w, s, op, true, p)
}

// WriteServerMessage writes message to w, considering that caller
// represents server side.
func WriteServerMessage(w io.Writer, op ws.OpCode, p []byte) error {
	return WriteMessage(w, ws.StateServerSide, op, p)
}

// WriteServerText is the same as WriteServerMessage with
// ws.OpText.
func WriteServerText(w io.Writer, p []byte) error {
	return WriteServerMessage(w, ws.OpText, p)
}

// WriteServerBinary is the same as WriteServerMessage with
// ws.OpBinary.
func WriteServerBinary(w io.Writer, p []byte) error {
	return WriteServerMessage(w, ws.OpBinary, p)
}

// WriteClientMessage writes message to w, considering that caller
// represents client side.
func WriteClientMessage(w io.Writer, op ws.OpCode, p []byte) error {
	return WriteMessage(w, ws.StateClientSide, op, p)
}

// WriteClientText is the same as WriteClientMessage with
// ws.OpText.
func WriteClientText(w io.Writer, p []byte) error {
	return WriteClientMessage(w, ws.OpText, p)
}

// WriteClientBinary is the same as WriteClientMessage with
// ws.OpBinary.
func WriteClientBinary(w io.Writer, p []byte) error {
	return WriteClientMessage(w, ws.OpBinary, p)
}

// HandleClientControlMessage handles control frame from conn and writes
// response when needed.
//
// It considers that caller represents server side.
func HandleClientControlMessage(conn io.Writer, msg Message) error {
	return HandleControlMessage(conn, ws.StateServerSide, msg)
}

// HandleServerControlMessage handles control frame from conn and writes
// response when needed.
//
// It considers that caller represents client side.
func HandleServerControlMessage(conn io.Writer, msg Message) error {
	return HandleControlMessage(conn, ws.StateClientSide, msg)
}

// HandleControlMessage handles message which was read by ReadMessage()
// functions.
//
// That is, it is expected, that payload is already unmasked and frame header
// were checked by ws.CheckHeader() call.
func HandleControlMessage(conn io.Writer, state ws.State, msg Message) error {
	return (ControlHandler{
		DisableSrcCiphering: true,
		Src:                 bytes.NewReader(msg.Payload),
		Dst:                 conn,
		State:               state,
	}).Handle(ws.Header{
		Length: int64(len(msg.Payload)),
		OpCode: msg.OpCode,
		Fin:    true,
		Masked: state.ServerSide(),
	})
}

// ControlFrameHandler returns FrameHandlerFunc for handling control frames.
// For more info see ControlHandler docs.
func ControlFrameHandler(w io.Writer, state ws.State) FrameHandlerFunc {
	return func(h ws.Header, r io.Reader) error {
		return (ControlHandler{
			DisableSrcCiphering: true,
			Src:                 r,
			Dst:                 w,
			State:               state,
		}).Handle(h)
	}
}

func readData(rw io.ReadWriter, s ws.State, want ws.OpCode) ([]byte, ws.OpCode, error) {
	controlHandler := ControlFrameHandler(rw, s)
	rd := Reader{
		Source:          rw,
		State:           s,
		CheckUTF8:       true,
		SkipHeaderCheck: false,
		OnIntermediate:  controlHandler,
	}
	for {
		hdr, err := rd.NextFrame()
		if err != nil {
			return nil, 0, err
		}
		if hdr.OpCode.IsControl() {
			if err := controlHandler(hdr, &rd); err != nil {
				return nil, 0, err
			}
			continue
		}
		if hdr.OpCode&want == 0 {
			if err := rd.Discard(); err != nil {
				return nil, 0, err
			}
			continue
		}

		bts, err := ioutil.ReadAll(&rd)

		return bts, hdr.OpCode, err
	}
}
