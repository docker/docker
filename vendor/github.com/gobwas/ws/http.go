package ws

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"

	"github.com/gobwas/httphead"
)

const (
	crlf          = "\r\n"
	colonAndSpace = ": "
	commaAndSpace = ", "
)

const (
	textHeadUpgrade = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
)

var (
	textHeadBadRequest          = statusText(http.StatusBadRequest)
	textHeadInternalServerError = statusText(http.StatusInternalServerError)
	textHeadUpgradeRequired     = statusText(http.StatusUpgradeRequired)

	textTailErrHandshakeBadProtocol   = errorText(ErrHandshakeBadProtocol)
	textTailErrHandshakeBadMethod     = errorText(ErrHandshakeBadMethod)
	textTailErrHandshakeBadHost       = errorText(ErrHandshakeBadHost)
	textTailErrHandshakeBadUpgrade    = errorText(ErrHandshakeBadUpgrade)
	textTailErrHandshakeBadConnection = errorText(ErrHandshakeBadConnection)
	textTailErrHandshakeBadSecAccept  = errorText(ErrHandshakeBadSecAccept)
	textTailErrHandshakeBadSecKey     = errorText(ErrHandshakeBadSecKey)
	textTailErrHandshakeBadSecVersion = errorText(ErrHandshakeBadSecVersion)
	textTailErrUpgradeRequired        = errorText(ErrHandshakeUpgradeRequired)
)

var (
	headerHost          = "Host"
	headerUpgrade       = "Upgrade"
	headerConnection    = "Connection"
	headerSecVersion    = "Sec-WebSocket-Version"
	headerSecProtocol   = "Sec-WebSocket-Protocol"
	headerSecExtensions = "Sec-WebSocket-Extensions"
	headerSecKey        = "Sec-WebSocket-Key"
	headerSecAccept     = "Sec-WebSocket-Accept"

	headerHostCanonical          = textproto.CanonicalMIMEHeaderKey(headerHost)
	headerUpgradeCanonical       = textproto.CanonicalMIMEHeaderKey(headerUpgrade)
	headerConnectionCanonical    = textproto.CanonicalMIMEHeaderKey(headerConnection)
	headerSecVersionCanonical    = textproto.CanonicalMIMEHeaderKey(headerSecVersion)
	headerSecProtocolCanonical   = textproto.CanonicalMIMEHeaderKey(headerSecProtocol)
	headerSecExtensionsCanonical = textproto.CanonicalMIMEHeaderKey(headerSecExtensions)
	headerSecKeyCanonical        = textproto.CanonicalMIMEHeaderKey(headerSecKey)
	headerSecAcceptCanonical     = textproto.CanonicalMIMEHeaderKey(headerSecAccept)
)

var (
	specHeaderValueUpgrade         = []byte("websocket")
	specHeaderValueConnection      = []byte("Upgrade")
	specHeaderValueConnectionLower = []byte("upgrade")
	specHeaderValueSecVersion      = []byte("13")
)

var (
	httpVersion1_0    = []byte("HTTP/1.0")
	httpVersion1_1    = []byte("HTTP/1.1")
	httpVersionPrefix = []byte("HTTP/")
)

type httpRequestLine struct {
	method, uri  []byte
	major, minor int
}

type httpResponseLine struct {
	major, minor int
	status       int
	reason       []byte
}

// httpParseRequestLine parses http request line like "GET / HTTP/1.0".
func httpParseRequestLine(line []byte) (req httpRequestLine, err error) {
	var proto []byte
	req.method, req.uri, proto = bsplit3(line, ' ')

	var ok bool
	req.major, req.minor, ok = httpParseVersion(proto)
	if !ok {
		err = ErrMalformedRequest
		return
	}

	return
}

func httpParseResponseLine(line []byte) (resp httpResponseLine, err error) {
	var (
		proto  []byte
		status []byte
	)
	proto, status, resp.reason = bsplit3(line, ' ')

	var ok bool
	resp.major, resp.minor, ok = httpParseVersion(proto)
	if !ok {
		return resp, ErrMalformedResponse
	}

	var convErr error
	resp.status, convErr = asciiToInt(status)
	if convErr != nil {
		return resp, ErrMalformedResponse
	}

	return resp, nil
}

// httpParseVersion parses major and minor version of HTTP protocol. It returns
// parsed values and true if parse is ok.
func httpParseVersion(bts []byte) (major, minor int, ok bool) {
	switch {
	case bytes.Equal(bts, httpVersion1_0):
		return 1, 0, true
	case bytes.Equal(bts, httpVersion1_1):
		return 1, 1, true
	case len(bts) < 8:
		return
	case !bytes.Equal(bts[:5], httpVersionPrefix):
		return
	}

	bts = bts[5:]

	dot := bytes.IndexByte(bts, '.')
	if dot == -1 {
		return
	}
	var err error
	major, err = asciiToInt(bts[:dot])
	if err != nil {
		return
	}
	minor, err = asciiToInt(bts[dot+1:])
	if err != nil {
		return
	}

	return major, minor, true
}

// httpParseHeaderLine parses HTTP header as key-value pair. It returns parsed
// values and true if parse is ok.
func httpParseHeaderLine(line []byte) (k, v []byte, ok bool) {
	colon := bytes.IndexByte(line, ':')
	if colon == -1 {
		return
	}

	k = btrim(line[:colon])
	// TODO(gobwas): maybe use just lower here?
	canonicalizeHeaderKey(k)

	v = btrim(line[colon+1:])

	return k, v, true
}

// httpGetHeader is the same as textproto.MIMEHeader.Get, except the thing,
// that key is already canonical. This helps to increase performance.
func httpGetHeader(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	v := h[key]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// The request MAY include a header field with the name
// |Sec-WebSocket-Protocol|.  If present, this value indicates one or more
// comma-separated subprotocol the client wishes to speak, ordered by
// preference.  The elements that comprise this value MUST be non-empty strings
// with characters in the range U+0021 to U+007E not including separator
// characters as defined in [RFC2616] and MUST all be unique strings.  The ABNF
// for the value of this header field is 1#token, where the definitions of
// constructs and rules are as given in [RFC2616].
func strSelectProtocol(h string, check func(string) bool) (ret string, ok bool) {
	ok = httphead.ScanTokens(strToBytes(h), func(v []byte) bool {
		if check(btsToString(v)) {
			ret = string(v)
			return false
		}
		return true
	})
	return
}
func btsSelectProtocol(h []byte, check func([]byte) bool) (ret string, ok bool) {
	var selected []byte
	ok = httphead.ScanTokens(h, func(v []byte) bool {
		if check(v) {
			selected = v
			return false
		}
		return true
	})
	if ok && selected != nil {
		return string(selected), true
	}
	return
}

func strSelectExtensions(h string, selected []httphead.Option, check func(httphead.Option) bool) ([]httphead.Option, bool) {
	return btsSelectExtensions(strToBytes(h), selected, check)
}

func btsSelectExtensions(h []byte, selected []httphead.Option, check func(httphead.Option) bool) ([]httphead.Option, bool) {
	s := httphead.OptionSelector{
		Flags: httphead.SelectUnique | httphead.SelectCopy,
		Check: check,
	}
	return s.Select(h, selected)
}

func httpWriteHeader(bw *bufio.Writer, key, value string) {
	httpWriteHeaderKey(bw, key)
	bw.WriteString(value)
	bw.WriteString(crlf)
}

func httpWriteHeaderBts(bw *bufio.Writer, key string, value []byte) {
	httpWriteHeaderKey(bw, key)
	bw.Write(value)
	bw.WriteString(crlf)
}

func httpWriteHeaderKey(bw *bufio.Writer, key string) {
	bw.WriteString(key)
	bw.WriteString(colonAndSpace)
}

func httpWriteUpgradeRequest(
	bw *bufio.Writer,
	u *url.URL,
	nonce []byte,
	protocols []string,
	extensions []httphead.Option,
	header HandshakeHeader,
) {
	bw.WriteString("GET ")
	bw.WriteString(u.RequestURI())
	bw.WriteString(" HTTP/1.1\r\n")

	httpWriteHeader(bw, headerHost, u.Host)

	httpWriteHeaderBts(bw, headerUpgrade, specHeaderValueUpgrade)
	httpWriteHeaderBts(bw, headerConnection, specHeaderValueConnection)
	httpWriteHeaderBts(bw, headerSecVersion, specHeaderValueSecVersion)

	// NOTE: write nonce bytes as a string to prevent heap allocation –
	// WriteString() copy given string into its inner buffer, unlike Write()
	// which may write p directly to the underlying io.Writer – which in turn
	// will lead to p escape.
	httpWriteHeader(bw, headerSecKey, btsToString(nonce))

	if len(protocols) > 0 {
		httpWriteHeaderKey(bw, headerSecProtocol)
		for i, p := range protocols {
			if i > 0 {
				bw.WriteString(commaAndSpace)
			}
			bw.WriteString(p)
		}
		bw.WriteString(crlf)
	}

	if len(extensions) > 0 {
		httpWriteHeaderKey(bw, headerSecExtensions)
		httphead.WriteOptions(bw, extensions)
		bw.WriteString(crlf)
	}

	if header != nil {
		header.WriteTo(bw)
	}

	bw.WriteString(crlf)
}

func httpWriteResponseUpgrade(bw *bufio.Writer, nonce []byte, hs Handshake, header HandshakeHeaderFunc) {
	bw.WriteString(textHeadUpgrade)

	httpWriteHeaderKey(bw, headerSecAccept)
	writeAccept(bw, nonce)
	bw.WriteString(crlf)

	if hs.Protocol != "" {
		httpWriteHeader(bw, headerSecProtocol, hs.Protocol)
	}
	if len(hs.Extensions) > 0 {
		httpWriteHeaderKey(bw, headerSecExtensions)
		httphead.WriteOptions(bw, hs.Extensions)
		bw.WriteString(crlf)
	}
	if header != nil {
		header(bw)
	}

	bw.WriteString(crlf)
}

func httpWriteResponseError(bw *bufio.Writer, err error, code int, header HandshakeHeaderFunc) {
	switch code {
	case http.StatusBadRequest:
		bw.WriteString(textHeadBadRequest)
	case http.StatusInternalServerError:
		bw.WriteString(textHeadInternalServerError)
	case http.StatusUpgradeRequired:
		bw.WriteString(textHeadUpgradeRequired)
	default:
		writeStatusText(bw, code)
	}

	// Write custom headers.
	if header != nil {
		header(bw)
	}

	switch err {
	case ErrHandshakeBadProtocol:
		bw.WriteString(textTailErrHandshakeBadProtocol)
	case ErrHandshakeBadMethod:
		bw.WriteString(textTailErrHandshakeBadMethod)
	case ErrHandshakeBadHost:
		bw.WriteString(textTailErrHandshakeBadHost)
	case ErrHandshakeBadUpgrade:
		bw.WriteString(textTailErrHandshakeBadUpgrade)
	case ErrHandshakeBadConnection:
		bw.WriteString(textTailErrHandshakeBadConnection)
	case ErrHandshakeBadSecAccept:
		bw.WriteString(textTailErrHandshakeBadSecAccept)
	case ErrHandshakeBadSecKey:
		bw.WriteString(textTailErrHandshakeBadSecKey)
	case ErrHandshakeBadSecVersion:
		bw.WriteString(textTailErrHandshakeBadSecVersion)
	case ErrHandshakeUpgradeRequired:
		bw.WriteString(textTailErrUpgradeRequired)
	case nil:
		bw.WriteString(crlf)
	default:
		writeErrorText(bw, err)
	}
}

func writeStatusText(bw *bufio.Writer, code int) {
	bw.WriteString("HTTP/1.1 ")
	bw.WriteString(strconv.Itoa(code))
	bw.WriteByte(' ')
	bw.WriteString(http.StatusText(code))
	bw.WriteString(crlf)
	bw.WriteString("Content-Type: text/plain; charset=utf-8")
	bw.WriteString(crlf)
}

func writeErrorText(bw *bufio.Writer, err error) {
	body := err.Error()
	bw.WriteString("Content-Length: ")
	bw.WriteString(strconv.Itoa(len(body)))
	bw.WriteString(crlf)
	bw.WriteString(crlf)
	bw.WriteString(body)
}

// httpError is like the http.Error with WebSocket context exception.
func httpError(w http.ResponseWriter, body string, code int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	w.Write([]byte(body))
}

// statusText is a non-performant status text generator.
// NOTE: Used only to generate constants.
func statusText(code int) string {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	writeStatusText(bw, code)
	bw.Flush()
	return buf.String()
}

// errorText is a non-performant error text generator.
// NOTE: Used only to generate constants.
func errorText(err error) string {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	writeErrorText(bw, err)
	bw.Flush()
	return buf.String()
}

// HandshakeHeader is the interface that writes both upgrade request or
// response headers into a given io.Writer.
type HandshakeHeader interface {
	io.WriterTo
}

// HandshakeHeaderString is an adapter to allow the use of headers represented
// by ordinary string as HandshakeHeader.
type HandshakeHeaderString string

// WriteTo implements HandshakeHeader (and io.WriterTo) interface.
func (s HandshakeHeaderString) WriteTo(w io.Writer) (int64, error) {
	n, err := io.WriteString(w, string(s))
	return int64(n), err
}

// HandshakeHeaderBytes is an adapter to allow the use of headers represented
// by ordinary slice of bytes as HandshakeHeader.
type HandshakeHeaderBytes []byte

// WriteTo implements HandshakeHeader (and io.WriterTo) interface.
func (b HandshakeHeaderBytes) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(b)
	return int64(n), err
}

// HandshakeHeaderFunc is an adapter to allow the use of headers represented by
// ordinary function as HandshakeHeader.
type HandshakeHeaderFunc func(io.Writer) (int64, error)

// WriteTo implements HandshakeHeader (and io.WriterTo) interface.
func (f HandshakeHeaderFunc) WriteTo(w io.Writer) (int64, error) {
	return f(w)
}

// HandshakeHeaderHTTP is an adapter to allow the use of http.Header as
// HandshakeHeader.
type HandshakeHeaderHTTP http.Header

// WriteTo implements HandshakeHeader (and io.WriterTo) interface.
func (h HandshakeHeaderHTTP) WriteTo(w io.Writer) (int64, error) {
	wr := writer{w: w}
	err := http.Header(h).Write(&wr)
	return wr.n, err
}

type writer struct {
	n int64
	w io.Writer
}

func (w *writer) WriteString(s string) (int, error) {
	n, err := io.WriteString(w.w, s)
	w.n += int64(n)
	return n, err
}

func (w *writer) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}
