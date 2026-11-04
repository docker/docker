package json

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unsafe"

	"github.com/aws/smithy-go/internal/serde"
)

// matches stdlib
const maxDepth = 10_000

const (
	inObject int8 = iota + 1
	inArray
)

type parseState uint8

const (
	stValue parseState = iota
	stObjectKey
	stObjectColon
	stObjectComma
	stObjectKeyAfterComma
	stArrayValue
	stArrayComma
)

func errUnexpectedToken(c byte) error {
	return fmt.Errorf("unexpected token '%c'", c)
}

// parser is a combined scanner+validator that pulls tokens from a JSON body.
// It integrates tokenization and grammar validation into a single loop to
// minimize function call overhead on the hot path.
type parser struct {
	p     []byte
	i     int
	stack serde.Stack[int8]
	done  bool
	state parseState

	// set by scanString: true if the last string token contained escapes
	escaped bool
}

func (p *parser) Next() ([]byte, error) {
	if p.done {
		return nil, io.EOF
	}

	for {
		// inline whitespace scanning + token identification
		i := p.i
		buf := p.p
		for ; i < len(buf); i++ {
			if charClass[buf[i]] != ccWhite {
				break
			}
		}
		if i >= len(buf) {
			p.i = i
			return nil, fmt.Errorf("unexpected end of JSON input")
		}

		c := buf[i]
		p.i = i

		// get the token
		var next []byte
		var err error
		if charClass[c] == ccDelim {
			p.i = i + 1
			next = buf[i : i+1]
		} else {
			switch c {
			case '"':
				next, err = p.scanString()
			case 't':
				next, err = p.scanLiteral("true")
			case 'f':
				next, err = p.scanLiteral("false")
			case 'n':
				next, err = p.scanLiteral("null")
			default:
				next, err = p.scanNumber()
			}
			if err != nil {
				return nil, err
			}
		}

		// validate grammar
		switch p.state {
		case stValue:
			switch next[0] {
			case '{':
				p.state = stObjectKey
				p.stack.Push(inObject)
				if p.stack.Len() > maxDepth {
					return nil, errors.New("exceeded max nesting depth")
				}
				return next, nil
			case '[':
				p.state = stArrayValue
				p.stack.Push(inArray)
				if p.stack.Len() > maxDepth {
					return nil, errors.New("exceeded max nesting depth")
				}
				return next, nil
			case ',', ':', '}', ']':
				return nil, errUnexpectedToken(next[0])
			}
			p.afterValue()
			return next, nil

		case stObjectKey:
			switch next[0] {
			case '"':
				p.state = stObjectColon
				return next, nil
			case '}':
				p.close()
				return next, nil
			default:
				return nil, errUnexpectedToken(next[0])
			}

		case stObjectColon:
			if next[0] != ':' {
				return nil, errUnexpectedToken(next[0])
			}
			p.state = stValue
			continue

		case stObjectComma:
			switch next[0] {
			case ',':
				p.state = stObjectKeyAfterComma
				continue
			case '}':
				p.close()
				return next, nil
			default:
				return nil, errUnexpectedToken(next[0])
			}

		case stObjectKeyAfterComma:
			if next[0] != '"' {
				return nil, errUnexpectedToken(next[0])
			}
			p.state = stObjectColon
			return next, nil

		case stArrayValue:
			if next[0] == ']' {
				p.close()
				return next, nil
			}
			// handle as value
			switch next[0] {
			case '{':
				p.state = stObjectKey
				p.stack.Push(inObject)
				if p.stack.Len() > maxDepth {
					return nil, errors.New("exceeded max nesting depth")
				}
				return next, nil
			case '[':
				p.state = stArrayValue
				p.stack.Push(inArray)
				if p.stack.Len() > maxDepth {
					return nil, errors.New("exceeded max nesting depth")
				}
				return next, nil
			case ',', ':', '}':
				return nil, errUnexpectedToken(next[0])
			}
			p.afterValue()
			return next, nil

		case stArrayComma:
			switch next[0] {
			case ',':
				p.state = stValue
				continue
			case ']':
				p.close()
				return next, nil
			default:
				return nil, errUnexpectedToken(next[0])
			}
		}
	}
}

func (p *parser) Skip() error {
	var depth int
	for {
		tok, err := p.Next()
		if err != nil {
			return err
		}
		switch tok[0] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}

func (p *parser) Value() (any, error) {
	tok, err := p.Next()
	if err != nil {
		return nil, err
	}
	return p.value(tok)
}

func (p *parser) value(tok []byte) (any, error) {
	switch tok[0] {
	case 'n':
		return nil, nil
	case 't':
		return true, nil
	case 'f':
		return false, nil
	case '"':
		return unquote(tok)
	case '{':
		m := map[string]any{}
		for {
			ktok, err := p.Next()
			if err != nil {
				return nil, err
			}
			if ktok[0] == '}' {
				return m, nil
			}
			key, err := unquote(ktok)
			if err != nil {
				return nil, err
			}
			val, err := p.Value()
			if err != nil {
				return nil, err
			}
			m[key] = val
		}
	case '[':
		var list []any
		for {
			tok, err := p.Next()
			if err != nil {
				return nil, err
			}
			if tok[0] == ']' {
				if list == nil {
					list = []any{}
				}
				return list, nil
			}
			val, err := p.value(tok)
			if err != nil {
				return nil, err
			}
			list = append(list, val)
		}
	default:
		return json.Number(tok), nil
	}
}

func (p *parser) close() {
	p.stack.Pop()
	p.afterValue()
}

func (p *parser) afterValue() {
	top := p.stack.Top()
	if top == nil {
		p.done = true
		return
	}

	switch *top {
	case inObject:
		p.state = stObjectComma
	case inArray:
		p.state = stArrayComma
	default:
		p.done = true
	}
}

// --- Integrated scanner methods ---

const (
	lo = uint64(0x0101010101010101)
	hi = uint64(0x8080808080808080)
)

func hasValue(v uint64, c byte) uint64 {
	x := v ^ (lo * uint64(c))
	return (x - lo) & ^x & hi
}

func hasLess(v uint64, n byte) uint64 {
	return (v - lo*uint64(n)) & ^v & hi
}

func (p *parser) scanString() ([]byte, error) {
	start := p.i
	i := p.i + 1 // skip opening "
	p.escaped = false

	// SWAR: scan 8 bytes at a time for '"', '\', or control chars (< 0x20)
	for i+8 <= len(p.p) {
		v := *(*uint64)(unsafe.Pointer(&p.p[i]))
		mask := hasLess(v, 0x20) | hasValue(v, '"') | hasValue(v, '\\')
		if mask != 0 {
			break
		}
		i += 8
	}

	for i < len(p.p) {
		c := p.p[i]
		if c < 0x20 {
			p.i = i
			return nil, fmt.Errorf("invalid control character at offset %d", i)
		}

		if c == '\\' {
			p.escaped = true
			p.i = i + 1
			if err := p.scanEscape(); err != nil {
				return nil, err
			}
			i = p.i
			continue
		}

		if c == '"' {
			i++
			p.i = i
			return p.p[start:i], nil
		}

		i++
	}
	p.i = i
	return nil, fmt.Errorf("unterminated string at offset %d", start)
}

func (p *parser) scanEscape() error {
	if p.i >= len(p.p) {
		return fmt.Errorf("unterminated escape at offset %d", p.i-1)
	}
	c := p.p[p.i]
	p.i++
	switch c {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return nil
	case 'u':
		return p.scanUnicodeEscape()
	default:
		return fmt.Errorf("invalid escape character '%c' at offset %d", c, p.i-1)
	}
}

func (p *parser) scanUnicodeEscape() error {
	if p.i+4 > len(p.p) {
		return fmt.Errorf("incomplete unicode escape at offset %d", p.i-2)
	}
	for _, c := range p.p[p.i : p.i+4] {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return fmt.Errorf("invalid character '%c' in unicode escape at offset %d", c, p.i-2)
		}
	}
	p.i += 4
	return nil
}

func (p *parser) scanNumber() ([]byte, error) {
	i := p.i
	if i < len(p.p) && p.p[i] == '-' {
		i++
	}
	digitStart := i
	for i < len(p.p) && p.p[i] >= '0' && p.p[i] <= '9' {
		i++
	}
	if i == digitStart {
		return nil, fmt.Errorf("unexpected token at offset %d", p.i)
	}
	if p.p[digitStart] == '0' && i-digitStart > 1 {
		return nil, fmt.Errorf("leading zeros not allowed at offset %d", digitStart)
	}

	if i < len(p.p) && p.p[i] == '.' {
		i++
		digitStart = i
		for i < len(p.p) && p.p[i] >= '0' && p.p[i] <= '9' {
			i++
		}
		if i == digitStart {
			return nil, fmt.Errorf("no digits after decimal point at offset %d", i)
		}
	}

	if i < len(p.p) && (p.p[i] == 'e' || p.p[i] == 'E') {
		i++
		if i < len(p.p) && (p.p[i] == '+' || p.p[i] == '-') {
			i++
		}
		digitStart = i
		for i < len(p.p) && p.p[i] >= '0' && p.p[i] <= '9' {
			i++
		}
		if i == digitStart {
			return nil, fmt.Errorf("no digits after exponent at offset %d", i)
		}
	}

	start := p.i
	p.i = i
	return p.p[start:i], nil
}

func (p *parser) scanLiteral(lit string) ([]byte, error) {
	end := p.i + len(lit)
	if end > len(p.p) || string(p.p[p.i:end]) != lit {
		return nil, fmt.Errorf("invalid literal at offset %d", p.i)
	}
	start := p.i
	p.i = end
	return p.p[start:end], nil
}

const (
	ccNone  byte = 0
	ccWhite byte = 1
	ccDelim byte = 2
)

var charClass = [256]byte{
	' ':  ccWhite,
	'\t': ccWhite,
	'\n': ccWhite,
	'\r': ccWhite,
	'{':  ccDelim,
	'}':  ccDelim,
	'[':  ccDelim,
	']':  ccDelim,
	':':  ccDelim,
	',':  ccDelim,
}
