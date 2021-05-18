package httphead

import "io"

var (
	comma     = []byte{','}
	equality  = []byte{'='}
	semicolon = []byte{';'}
	quote     = []byte{'"'}
	escape    = []byte{'\\'}
)

// WriteOptions write options list to the dest.
// It uses the same form as {Scan,Parse}Options functions:
// values = 1#value
// value = token *( ";" param )
// param = token [ "=" (token | quoted-string) ]
//
// It wraps valuse into the quoted-string sequence if it contains any
// non-token characters.
func WriteOptions(dest io.Writer, options []Option) (n int, err error) {
	w := writer{w: dest}
	for i, opt := range options {
		if i > 0 {
			w.write(comma)
		}

		writeTokenSanitized(&w, opt.Name)

		for _, p := range opt.Parameters.data() {
			w.write(semicolon)
			writeTokenSanitized(&w, p.key)
			if len(p.value) != 0 {
				w.write(equality)
				writeTokenSanitized(&w, p.value)
			}
		}
	}
	return w.result()
}

// writeTokenSanitized writes token as is or as quouted string if it contains
// non-token characters.
//
// Note that is is not expects LWS sequnces be in s, cause LWS is used only as
// header field continuation:
// "A CRLF is allowed in the definition of TEXT only as part of a header field
// continuation. It is expected that the folding LWS will be replaced with a
// single SP before interpretation of the TEXT value."
// See https://tools.ietf.org/html/rfc2616#section-2
//
// That is we sanitizing s for writing, so there could not be any header field
// continuation.
// That is any CRLF will be escaped as any other control characters not allowd in TEXT.
func writeTokenSanitized(bw *writer, bts []byte) {
	var qt bool
	var pos int
	for i := 0; i < len(bts); i++ {
		c := bts[i]
		if !OctetTypes[c].IsToken() && !qt {
			qt = true
			bw.write(quote)
		}
		if OctetTypes[c].IsControl() || c == '"' {
			if !qt {
				qt = true
				bw.write(quote)
			}
			bw.write(bts[pos:i])
			bw.write(escape)
			bw.write(bts[i : i+1])
			pos = i + 1
		}
	}
	if !qt {
		bw.write(bts)
	} else {
		bw.write(bts[pos:])
		bw.write(quote)
	}
}

type writer struct {
	w   io.Writer
	n   int
	err error
}

func (w *writer) write(p []byte) {
	if w.err != nil {
		return
	}
	var n int
	n, w.err = w.w.Write(p)
	w.n += n
	return
}

func (w *writer) result() (int, error) {
	return w.n, w.err
}
