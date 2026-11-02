package json

import (
	"encoding/base64"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/document"
	"github.com/aws/smithy-go/internal/serde"
	smithytime "github.com/aws/smithy-go/time"
	"github.com/aws/smithy-go/traits"
	"github.com/aws/smithy-go/transport/http/protocol/internal/json/internal/stdlib"
)

type ctxKind int8

const (
	ctxList ctxKind = iota + 1
	ctxMap
	ctxStruct
	ctxUnion
)

type deserCtx struct {
	kind   ctxKind
	schema *smithy.Schema // for ctxStruct
}

// ShapeDeserializer implements unmarshaling of JSON into Smithy shapes.
type ShapeDeserializer struct {
	p    parser
	head serde.Stack[deserCtx]
	opts Options

	peeked        []byte
	peekedEscaped bool

	arena serde.StringArena
}

var deserPool = sync.Pool{
	New: func() any {
		return &ShapeDeserializer{
			p:    parser{stack: serde.NewStack[int8]()},
			head: serde.NewStack[deserCtx](),
		}
	},
}

// NewShapeDeserializer creates a new ShapeDeserializer.
func NewShapeDeserializer(p []byte, opts ...func(*Options)) *ShapeDeserializer {
	o := Options{}
	for _, fn := range opts {
		fn(&o)
	}
	d := deserPool.Get().(*ShapeDeserializer)
	d.p.p = p
	d.p.i = 0
	d.p.state = stValue
	d.p.done = false
	d.p.stack.Reset()
	d.head.Reset()
	d.peeked = nil
	d.opts = o
	d.arena.Reset(len(p) / serde.ArenaPayloadFactor)
	return d
}

// Close returns the deserializer to the pool for reuse.
func (d *ShapeDeserializer) Close() {
	deserPool.Put(d)
}

var _ smithy.ShapeDeserializer = (*ShapeDeserializer)(nil)

func (d *ShapeDeserializer) next() ([]byte, error) {
	if d.peeked != nil {
		peeked := d.peeked
		d.peeked = nil
		d.p.escaped = d.peekedEscaped
		return peeked, nil
	}
	return d.p.Next()
}

func (d *ShapeDeserializer) peek() ([]byte, error) {
	if d.peeked != nil {
		return d.peeked, nil
	}
	tok, err := d.p.Next()
	if err != nil {
		return nil, err
	}
	d.peeked = tok
	d.peekedEscaped = d.p.escaped
	return tok, nil
}

// ReadNil implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadNil(s *smithy.Schema) (bool, error) {
	tok, err := d.peek()
	if err != nil {
		return false, err
	}
	if isN(tok) {
		d.peeked = nil
		return true, nil
	}
	return false, nil
}

// ReadInt8 implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadInt8(s *smithy.Schema, v *int8) error {
	n, err := d.readInt(math.MinInt8, math.MaxInt8)
	*v = int8(n)
	return err
}

// ReadInt16 implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadInt16(s *smithy.Schema, v *int16) error {
	n, err := d.readInt(math.MinInt16, math.MaxInt16)
	*v = int16(n)
	return err
}

// ReadInt32 implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadInt32(s *smithy.Schema, v *int32) error {
	n, err := d.readInt(math.MinInt32, math.MaxInt32)
	*v = int32(n)
	return err
}

// ReadInt64 implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadInt64(s *smithy.Schema, v *int64) error {
	n, err := d.readInt(math.MinInt64, math.MaxInt64)
	*v = n
	return err
}

func (d *ShapeDeserializer) readInt(min, max int64) (int64, error) {
	tok, err := d.next()
	if err != nil {
		return 0, err
	}

	if isS(tok) || isLCB(tok) || isLSB(tok) {
		return 0, fmt.Errorf("expected number, got %s", tok)
	}

	n, ok := parseInt(tok)
	if !ok {
		return 0, fmt.Errorf("invalid int: %s", tok)
	}

	if n < min || n > max {
		return 0, fmt.Errorf("int %d exceeds range [%d, %d]", n, min, max)
	}

	return n, nil
}

// parseInt parses a decimal int directly from bytes, avoiding string alloc.
func parseInt(b []byte) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	neg := false
	if b[0] == '-' {
		neg = true
		b = b[1:]
		if len(b) == 0 {
			return 0, false
		}
	}

	const cutoff = math.MaxUint64/10 + 1
	var n uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		if n >= cutoff {
			return 0, false
		}
		nn := n*10 + uint64(c-'0')
		if nn < n {
			return 0, false
		}
		n = nn
		if n > uint64(math.MaxInt64) {
			if neg && n == uint64(math.MaxInt64)+1 {
				return math.MinInt64, true
			}
			return 0, false
		}
	}

	if neg {
		return -int64(n), true
	}
	return int64(n), true
}

// ReadFloat32 implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadFloat32(s *smithy.Schema, v *float32) error {
	n, err := d.readFloat()
	*v = float32(n)
	return err
}

// ReadFloat64 implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadFloat64(s *smithy.Schema, v *float64) error {
	n, err := d.readFloat()
	*v = n
	return err
}

func (d *ShapeDeserializer) readFloat() (float64, error) {
	tok, err := d.next()
	if err != nil {
		return 0, err
	}

	if isS(tok) {
		s, err := unquote(tok)
		if err != nil {
			return 0, err
		}
		switch {
		case strings.EqualFold(s, "NaN"):
			return math.NaN(), nil
		case strings.EqualFold(s, "Infinity"):
			return math.Inf(1), nil
		case strings.EqualFold(s, "-Infinity"):
			return math.Inf(-1), nil
		default:
			return 0, fmt.Errorf("unexpected string value for float: %s", s)
		}
	}

	// fast path: if it's a plain integer, parse directly without alloc
	if n, ok := parseInt(tok); ok {
		return float64(n), nil
	}
	return strconv.ParseFloat(string(tok), 64)
}

// ReadBool implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadBool(s *smithy.Schema, v *bool) error {
	tok, err := d.next()
	if err != nil {
		return err
	}

	switch {
	case isT(tok):
		*v = true
		return nil
	case isF(tok):
		*v = false
		return nil
	default:
		return fmt.Errorf("expected bool, got %s", tok)
	}
}

// ReadString implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadString(s *smithy.Schema, v *string) error {
	tok, err := d.next()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}

	if !isS(tok) {
		return fmt.Errorf("expected string, got %s", tok)
	}

	if !d.p.escaped {
		*v = d.arena.String(tok[1 : len(tok)-1])
		return nil
	}

	sv, err := d.unquoteArena(tok)
	if err != nil {
		return err
	}

	*v = sv
	return nil
}

// ReadTime implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadTime(schema *smithy.Schema, v *time.Time) error {
	format := "epoch-seconds"
	if t, ok := smithy.SchemaTrait[*traits.TimestampFormat](schema); ok {
		format = t.Format
	}

	switch format {
	case "epoch-seconds":
		n, err := d.readFloat()
		if err != nil {
			return err
		}
		*v = smithytime.ParseEpochSeconds(n)
		return nil
	case "date-time":
		var s string
		if err := d.ReadString(schema, &s); err != nil {
			return err
		}
		t, err := smithytime.ParseDateTime(s)
		if err != nil {
			return err
		}
		*v = t
		return nil
	case "http-date":
		var s string
		if err := d.ReadString(schema, &s); err != nil {
			return err
		}
		t, err := smithytime.ParseHTTPDate(s)
		if err != nil {
			return err
		}
		*v = t
		return nil
	default:
		return fmt.Errorf("unknown timestamp format: %s", format)
	}
}

// ReadBlob implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadBlob(s *smithy.Schema, v *[]byte) error {
	if isNil, err := d.ReadNil(s); isNil || err != nil {
		return err
	}

	tok, err := d.next()
	if err != nil {
		return err
	}

	if !isS(tok) {
		return fmt.Errorf("expected string, got %s", tok)
	}

	sv, err := unquote(tok)
	if err != nil {
		return err
	}

	b, err := base64.StdEncoding.DecodeString(sv)
	if err != nil {
		return fmt.Errorf("decode base64 blob: %w", err)
	}

	*v = b
	return nil
}

// ReadList implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadList(s *smithy.Schema) error {
	tok, err := d.next()
	if err != nil {
		return err
	}
	if !isLSB(tok) {
		return fmt.Errorf("expected '[', got %s", tok)
	}
	d.head.Push(deserCtx{kind: ctxList})
	return nil
}

// ReadListItem implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadListItem(s *smithy.Schema) (bool, error) {
	tok, err := d.peek()
	if err != nil {
		return false, err
	}
	if isRSB(tok) {
		d.peeked = nil
		d.head.Pop()
		return false, nil
	}
	return true, nil
}

// ReadMap implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadMap(s *smithy.Schema) error {
	tok, err := d.next()
	if err != nil {
		return err
	}
	if !isLCB(tok) {
		return fmt.Errorf("expected '{', got %s", tok)
	}
	d.head.Push(deserCtx{kind: ctxMap})
	return nil
}

// ReadMapKey implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadMapKey(s *smithy.Schema) (string, bool, error) {
	tok, err := d.next()
	if err != nil {
		return "", false, err
	}
	if isRCB(tok) {
		d.head.Pop()
		return "", false, nil
	}

	if !d.p.escaped {
		return d.arena.String(tok[1 : len(tok)-1]), true, nil
	}

	key, err := d.unquoteArena(tok)
	if err != nil {
		return "", false, err
	}
	return key, true, nil
}

// ReadStruct implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadStruct(s *smithy.Schema) error {
	if isNil, err := d.ReadNil(s); isNil || err != nil {
		return err
	}

	tok, err := d.next()
	if err != nil {
		return err
	}
	if !isLCB(tok) {
		return fmt.Errorf("expected '{', got %s", tok)
	}
	d.head.Push(deserCtx{kind: ctxStruct, schema: s})
	return nil
}

// ReadStructMember implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadStructMember() (*smithy.Schema, error) {
	tok, err := d.next()
	if err != nil {
		return nil, err
	}
	if isRCB(tok) {
		d.head.Pop()
		return nil, nil
	}

	top := d.head.Top()
	if top == nil || top.kind != ctxStruct {
		return nil, fmt.Errorf("ReadStructMember called without ReadStruct?")
	}

	member, err := memberFromToken(top.schema, tok, d.p.escaped)
	if err != nil {
		return nil, err
	}

	if member == nil && d.opts.UseJSONName {
		key, err := unquote(tok)
		if err != nil {
			return nil, err
		}
		for _, m := range top.schema.Members() {
			if jn, ok := smithy.SchemaTrait[*traits.JSONName](m); ok && jn.Name == key {
				member = m
				break
			}
		}
	}
	if member == nil {
		if err := d.p.Skip(); err != nil {
			return nil, err
		}
		return d.ReadStructMember()
	}

	// inline null check to avoid function call overhead of ReadNil
	ptok, err := d.peek()
	if err != nil {
		return nil, err
	}
	if isN(ptok) {
		d.peeked = nil
		return d.ReadStructMember()
	}

	return member, nil
}

// ReadUnion implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadUnion(s *smithy.Schema) (*smithy.Schema, error) {
	if top := d.head.Top(); top == nil || top.kind != ctxUnion || top.schema != s {
		if isNil, err := d.ReadNil(s); isNil || err != nil {
			return nil, err
		}

		tok, err := d.next()
		if err != nil {
			return nil, err
		}
		if !isLCB(tok) {
			return nil, fmt.Errorf("expected '{', got %s", tok)
		}
		d.head.Push(deserCtx{kind: ctxUnion, schema: s})
	}

	for {
		tok, err := d.next()
		if err != nil {
			return nil, err
		}
		if isRCB(tok) {
			d.head.Pop()
			return nil, nil
		}

		member, err := memberFromToken(s, tok, d.p.escaped)
		if err != nil {
			return nil, err
		}

		if member == nil && d.opts.UseJSONName {
			key, err := unquote(tok)
			if err != nil {
				return nil, err
			}
			for _, m := range s.Members() {
				if jn, ok := smithy.SchemaTrait[*traits.JSONName](m); ok && jn.Name == key {
					member = m
					break
				}
			}
		}
		if member == nil {
			if err := d.p.Skip(); err != nil {
				return nil, err
			}
			continue
		}

		// inline null check
		ptok, err := d.peek()
		if err != nil {
			return nil, err
		}
		if isN(ptok) {
			d.peeked = nil
			continue
		}

		return member, nil
	}
}

// ReadDocument reads a JSON value into a document Value.
func (d *ShapeDeserializer) ReadDocument(schema *smithy.Schema, v *document.Value) error {
	tok, err := d.next()
	if err != nil {
		return err
	}
	vv, err := d.p.value(tok)
	if err != nil {
		return err
	}
	*v = document.Opaque{Value: vv}
	return nil
}

func unquote(tok []byte) (string, error) {
	if s, ok := stdlib.UnquoteBytes(tok); ok {
		return string(s), nil
	}
	return "", fmt.Errorf("cannot unquote %s", tok)
}

func (d *ShapeDeserializer) unquoteArena(tok []byte) (string, error) {
	if s, ok := stdlib.UnquoteBytes(tok); ok {
		return d.arena.String(s), nil
	}
	return "", fmt.Errorf("cannot unquote %s", tok)
}

func memberFromToken(s *smithy.Schema, tok []byte, escaped bool) (*smithy.Schema, error) {
	inner := tok[1 : len(tok)-1]
	if m := memberByBytes(s, inner); m != nil {
		return m, nil
	}

	// if the string had no escapes, the raw bytes ARE the unquoted form --
	// no point re-trying the lookup
	if !escaped {
		return errorMessageMember(s, inner), nil
	}

	unq, err := unquote(tok)
	if err != nil {
		return nil, err
	}

	if m := s.Member(unq); m != nil {
		return m, nil
	}
	return errorMessageMember(s, []byte(unq)), nil
}

// Services do not consistently use the casing that is modeled for the message
// part of errors, handle that here.
func errorMessageMember(s *smithy.Schema, name []byte) *smithy.Schema {
	var alt string
	switch string(name) {
	case "message":
		alt = "Message"
	case "Message":
		alt = "message"
	default:
		return nil
	}

	if _, ok := smithy.SchemaTrait[*traits.Error](s); !ok {
		return nil
	}
	return s.Members()[alt]
}

func isN(tok []byte) bool   { return tok[0] == 'n' }
func isT(tok []byte) bool   { return tok[0] == 't' }
func isF(tok []byte) bool   { return tok[0] == 'f' }
func isS(tok []byte) bool   { return tok[0] == '"' }
func isLCB(tok []byte) bool { return tok[0] == '{' }
func isRCB(tok []byte) bool { return tok[0] == '}' }
func isLSB(tok []byte) bool { return tok[0] == '[' }
func isRSB(tok []byte) bool { return tok[0] == ']' }

// ReadBigInt is unimplemented and will return an error.
func (d *ShapeDeserializer) ReadBigInt(_ *smithy.Schema, _ *big.Int) error {
	return fmt.Errorf("unimplemented")
}

// ReadBigFloat is unimplemented and will return an error.
func (d *ShapeDeserializer) ReadBigFloat(_ *smithy.Schema, _ *big.Float) error {
	return fmt.Errorf("unimplemented")
}

// DirectReadStruct is a concrete-type fast path that avoids interface dispatch.
// It skips the head stack and reads struct members directly.
func (d *ShapeDeserializer) DirectReadStruct(schema *smithy.Schema, memberFn func(*smithy.Schema) error) error {
	// null check
	tok, err := d.peek()
	if err != nil {
		return err
	}
	if isN(tok) {
		d.peeked = nil
		return nil
	}

	// consume '{'
	tok, err = d.next()
	if err != nil {
		return err
	}
	if !isLCB(tok) {
		return fmt.Errorf("expected '{', got %s", tok)
	}

	for {
		tok, err = d.next()
		if err != nil {
			return err
		}
		if isRCB(tok) {
			return nil
		}

		keyEscaped := d.p.escaped

		member, err := memberFromToken(schema, tok, keyEscaped)
		if err != nil {
			return err
		}

		if member == nil && d.opts.UseJSONName {
			key, qerr := unquote(tok)
			if qerr != nil {
				return qerr
			}
			for _, m := range schema.Members() {
				if jn, ok := smithy.SchemaTrait[*traits.JSONName](m); ok && jn.Name == key {
					member = m
					break
				}
			}
		}
		if member == nil {
			if err := d.p.Skip(); err != nil {
				return err
			}
			continue
		}

		// inline null check for the value
		ptok, err := d.peek()
		if err != nil {
			return err
		}
		if isN(ptok) {
			d.peeked = nil
			continue
		}

		if err := memberFn(member); err != nil {
			return err
		}
	}
}

// DirectReadUnion is a concrete-type fast path that avoids interface dispatch.
// It opens the union object, finds the single non-null member, calls memberFn,
// then drains to the closing brace.
func (d *ShapeDeserializer) DirectReadUnion(schema *smithy.Schema, memberFn func(*smithy.Schema) error) error {
	// open phase: consume '{' (or 'null')
	tok, err := d.next()
	if err != nil {
		return err
	}
	if isN(tok) {
		return nil
	}
	if !isLCB(tok) {
		return fmt.Errorf("expected '{', got %s", tok)
	}

	// find the single non-null member
	var member *smithy.Schema
	for {
		tok, err = d.next()
		if err != nil {
			return err
		}
		if isRCB(tok) {
			return nil
		}

		keyEscaped := d.p.escaped

		ptok, err := d.peek()
		if err != nil {
			return err
		}
		if isN(ptok) {
			d.peeked = nil
			continue
		}

		member, err = memberFromToken(schema, tok, keyEscaped)
		if err != nil {
			return err
		}

		if member == nil && d.opts.UseJSONName {
			key, err := unquote(tok)
			if err != nil {
				return err
			}
			for _, m := range schema.Members() {
				if jn, ok := smithy.SchemaTrait[*traits.JSONName](m); ok && jn.Name == key {
					member = m
					break
				}
			}
		}
		if member == nil {
			if err := d.p.Skip(); err != nil {
				return err
			}
			continue
		}

		break
	}

	// call the member function
	if err := memberFn(member); err != nil {
		return err
	}

	// drain remaining members to closing '}'
	for {
		tok, err = d.next()
		if err != nil {
			return err
		}
		if isRCB(tok) {
			return nil
		}

		// skip any extra keys (lenient: tolerate services sending extra fields)
		if err := d.p.Skip(); err != nil {
			return err
		}
	}
}

// DirectReadMap is a concrete-type fast path that avoids interface dispatch.
// It skips the head stack and reads map entries directly.
func (d *ShapeDeserializer) DirectReadMap(schema *smithy.Schema, memberFn func(string) error) error {
	tok, err := d.next()
	if err != nil {
		return err
	}
	if !isLCB(tok) {
		return fmt.Errorf("expected '{', got %s", tok)
	}

	for {
		tok, err = d.next()
		if err != nil {
			return err
		}
		if isRCB(tok) {
			return nil
		}

		var key string
		if !d.p.escaped {
			key = d.arena.String(tok[1 : len(tok)-1])
		} else {
			key, err = d.unquoteArena(tok)
			if err != nil {
				return err
			}
		}

		if err := memberFn(key); err != nil {
			return err
		}
	}
}

// DirectReadList is a concrete-type fast path that avoids interface dispatch.
// It skips the head stack and reads list elements directly using peek.
func (d *ShapeDeserializer) DirectReadList(schema *smithy.Schema, memberFn func() error) error {
	tok, err := d.next()
	if err != nil {
		return err
	}
	if !isLSB(tok) {
		return fmt.Errorf("expected '[', got %s", tok)
	}

	for {
		tok, err = d.peek()
		if err != nil {
			return err
		}
		if isRSB(tok) {
			d.peeked = nil
			return nil
		}
		if err := memberFn(); err != nil {
			return err
		}
	}
}
