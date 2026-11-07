package msgp

import (
	"reflect"
	"strconv"
)

const resumableDefault = false

var (
	// ErrShortBytes is returned when the
	// slice being decoded is too short to
	// contain the contents of the message
	ErrShortBytes error = errShort{}

	// ErrRecursion is returned when the maximum recursion limit is reached for an operation.
	// This should only realistically be seen on adversarial data trying to exhaust the stack.
	ErrRecursion error = errRecursion{}

	// ErrLimitExceeded is returned when a set limit is exceeded.
	// Limits can be set on the Reader to prevent excessive memory usage by adversarial data.
	ErrLimitExceeded error = errLimitExceeded{}

	// this error is only returned
	// if we reach code that should
	// be unreachable
	fatal error = errFatal{}
)

// Error is the interface satisfied
// by all of the errors that originate
// from this package.
type Error interface {
	error

	// Resumable returns whether
	// or not the error means that
	// the stream of data is malformed
	// and the information is unrecoverable.
	Resumable() bool
}

// contextError allows msgp Error instances to be enhanced with additional
// context about their origin.
type contextError interface {
	Error

	// withContext must not modify the error instance - it must clone and
	// return a new error with the context added.
	withContext(ctx string) error
}

// Cause returns the underlying cause of an error that has been wrapped
// with additional context.
func Cause(e error) error {
	out := e
	if e, ok := e.(errWrapped); ok && e.cause != nil {
		out = e.cause
	}
	return out
}

// Resumable returns whether or not the error means that the stream of data is
// malformed and the information is unrecoverable.
func Resumable(e error) bool {
	if e, ok := e.(Error); ok {
		return e.Resumable()
	}
	return resumableDefault
}

// WrapError wraps an error with additional context that allows the part of the
// serialized type that caused the problem to be identified. Underlying errors
// can be retrieved using Cause()
//
// The input error is not modified - a new error should be returned.
//
// ErrShortBytes is not wrapped with any context due to backward compatibility
// issues with the public API.
func WrapError(err error, ctx ...interface{}) error {
	switch e := err.(type) {
	case errShort:
		return e
	case contextError:
		return e.withContext(ctxString(ctx))
	default:
		return errWrapped{cause: err, ctx: ctxString(ctx)}
	}
}

func addCtx(ctx, add string) string {
	if ctx != "" {
		return add + "/" + ctx
	} else {
		return add
	}
}

// errWrapped allows arbitrary errors passed to WrapError to be enhanced with
// context and unwrapped with Cause()
type errWrapped struct {
	cause error
	ctx   string
}

func (e errWrapped) Error() string {
	if e.ctx != "" {
		return e.cause.Error() + " at " + e.ctx
	} else {
		return e.cause.Error()
	}
}

func (e errWrapped) Resumable() bool {
	if e, ok := e.cause.(Error); ok {
		return e.Resumable()
	}
	return resumableDefault
}

// Unwrap returns the cause.
func (e errWrapped) Unwrap() error { return e.cause }

type errShort struct{}

func (e errShort) Error() string   { return "msgp: too few bytes left to read object" }
func (e errShort) Resumable() bool { return false }

type errFatal struct {
	ctx string
}

func (f errFatal) Error() string {
	out := "msgp: fatal decoding error (unreachable code)"
	if f.ctx != "" {
		out += " at " + f.ctx
	}
	return out
}

func (f errFatal) Resumable() bool { return false }

func (f errFatal) withContext(ctx string) error { f.ctx = addCtx(f.ctx, ctx); return f }

type errRecursion struct{}

func (e errRecursion) Error() string   { return "msgp: recursion limit reached" }
func (e errRecursion) Resumable() bool { return false }

type errLimitExceeded struct{}

func (e errLimitExceeded) Error() string   { return "msgp: configured reader limit exceeded" }
func (e errLimitExceeded) Resumable() bool { return false }

// ArrayError is an error returned
// when decoding a fix-sized array
// of the wrong size
type ArrayError struct {
	Wanted uint32
	Got    uint32
	ctx    string
}

// Error implements the error interface
func (a ArrayError) Error() string {
	out := "msgp: wanted array of size " + strconv.Itoa(int(a.Wanted)) + "; got " + strconv.Itoa(int(a.Got))
	if a.ctx != "" {
		out += " at " + a.ctx
	}
	return out
}

// Resumable is always 'true' for ArrayErrors
func (a ArrayError) Resumable() bool { return true }

func (a ArrayError) withContext(ctx string) error { a.ctx = addCtx(a.ctx, ctx); return a }

// IntOverflow is returned when a call
// would downcast an integer to a type
// with too few bits to hold its value.
type IntOverflow struct {
	Value         int64 // the value of the integer
	FailedBitsize int   // the bit size that the int64 could not fit into
	ctx           string
}

// Error implements the error interface
func (i IntOverflow) Error() string {
	str := "msgp: " + strconv.FormatInt(i.Value, 10) + " overflows int" + strconv.Itoa(i.FailedBitsize)
	if i.ctx != "" {
		str += " at " + i.ctx
	}
	return str
}

// Resumable is always 'true' for overflows
func (i IntOverflow) Resumable() bool { return true }

func (i IntOverflow) withContext(ctx string) error { i.ctx = addCtx(i.ctx, ctx); return i }

// UintOverflow is returned when a call
// would downcast an unsigned integer to a type
// with too few bits to hold its value
type UintOverflow struct {
	Value         uint64 // value of the uint
	FailedBitsize int    // the bit size that couldn't fit the value
	ctx           string
}

// Error implements the error interface
func (u UintOverflow) Error() string {
	str := "msgp: " + strconv.FormatUint(u.Value, 10) + " overflows uint" + strconv.Itoa(u.FailedBitsize)
	if u.ctx != "" {
		str += " at " + u.ctx
	}
	return str
}

// Resumable is always 'true' for overflows
func (u UintOverflow) Resumable() bool { return true }

func (u UintOverflow) withContext(ctx string) error { u.ctx = addCtx(u.ctx, ctx); return u }

// InvalidTimestamp is returned when an invalid timestamp is encountered
type InvalidTimestamp struct {
	Nanos       int64 // value of the nano, if invalid
	FieldLength int   // Unexpected field length.
	ctx         string
}

// Error implements the error interface
func (u InvalidTimestamp) Error() (str string) {
	if u.Nanos > 0 {
		str = "msgp: timestamp nanosecond field value " + strconv.FormatInt(u.Nanos, 10) + " exceeds maximum allows of 999999999"
	} else if u.FieldLength >= 0 {
		str = "msgp: invalid timestamp field length " + strconv.FormatInt(int64(u.FieldLength), 10) + " - must be 4, 8 or 12"
	}
	if u.ctx != "" {
		str += " at " + u.ctx
	}
	return str
}

// Resumable is always 'true' for overflows
func (u InvalidTimestamp) Resumable() bool { return true }

func (u InvalidTimestamp) withContext(ctx string) error { u.ctx = addCtx(u.ctx, ctx); return u }

// UintBelowZero is returned when a call
// would cast a signed integer below zero
// to an unsigned integer.
type UintBelowZero struct {
	Value int64 // value of the incoming int
	ctx   string
}

// Error implements the error interface
func (u UintBelowZero) Error() string {
	str := "msgp: attempted to cast int " + strconv.FormatInt(u.Value, 10) + " to unsigned"
	if u.ctx != "" {
		str += " at " + u.ctx
	}
	return str
}

// Resumable is always 'true' for overflows
func (u UintBelowZero) Resumable() bool { return true }

func (u UintBelowZero) withContext(ctx string) error {
	u.ctx = ctx
	return u
}

// A TypeError is returned when a particular
// decoding method is unsuitable for decoding
// a particular MessagePack value.
type TypeError struct {
	Method  Type // Type expected by method
	Encoded Type // Type actually encoded

	ctx string
}

// Error implements the error interface
func (t TypeError) Error() string {
	out := "msgp: attempted to decode type " + quoteStr(t.Encoded.String()) + " with method for " + quoteStr(t.Method.String())
	if t.ctx != "" {
		out += " at " + t.ctx
	}
	return out
}

// Resumable returns 'true' for TypeErrors
func (t TypeError) Resumable() bool { return true }

func (t TypeError) withContext(ctx string) error { t.ctx = addCtx(t.ctx, ctx); return t }

// returns either InvalidPrefixError or
// TypeError depending on whether or not
// the prefix is recognized
func badPrefix(want Type, lead byte) error {
	t := getType(lead)
	if t == InvalidType {
		return InvalidPrefixError(lead)
	}
	return TypeError{Method: want, Encoded: t}
}

// InvalidPrefixError is returned when a bad encoding
// uses a prefix that is not recognized in the MessagePack standard.
// This kind of error is unrecoverable.
type InvalidPrefixError byte

// Error implements the error interface
func (i InvalidPrefixError) Error() string {
	return "msgp: unrecognized type prefix 0x" + strconv.FormatInt(int64(i), 16)
}

// Resumable returns 'false' for InvalidPrefixErrors
func (i InvalidPrefixError) Resumable() bool { return false }

// ErrUnsupportedType is returned
// when a bad argument is supplied
// to a function that takes `interface{}`.
type ErrUnsupportedType struct {
	T reflect.Type

	ctx string
}

// Error implements error
func (e *ErrUnsupportedType) Error() string {
	out := "msgp: type " + quoteStr(e.T.String()) + " not supported"
	if e.ctx != "" {
		out += " at " + e.ctx
	}
	return out
}

// Resumable returns 'true' for ErrUnsupportedType
func (e *ErrUnsupportedType) Resumable() bool { return true }

func (e *ErrUnsupportedType) withContext(ctx string) error {
	o := *e
	o.ctx = addCtx(o.ctx, ctx)
	return &o
}

// simpleQuoteStr is a simplified version of strconv.Quote for TinyGo,
// which takes up a lot less code space by escaping all non-ASCII
// (UTF-8) bytes with \x.  Saves about 4k of code size
// (unicode tables, needed for IsPrint(), are big).
// It lives in errors.go just so we can test it in errors_test.go
func simpleQuoteStr(s string) string {
	const (
		lowerhex = "0123456789abcdef"
	)

	sb := make([]byte, 0, len(s)+2)

	sb = append(sb, `"`...)

l: // loop through string bytes (not UTF-8 characters)
	for i := 0; i < len(s); i++ {
		b := s[i]
		// specific escape chars
		switch b {
		case '\\':
			sb = append(sb, `\\`...)
		case '"':
			sb = append(sb, `\"`...)
		case '\a':
			sb = append(sb, `\a`...)
		case '\b':
			sb = append(sb, `\b`...)
		case '\f':
			sb = append(sb, `\f`...)
		case '\n':
			sb = append(sb, `\n`...)
		case '\r':
			sb = append(sb, `\r`...)
		case '\t':
			sb = append(sb, `\t`...)
		case '\v':
			sb = append(sb, `\v`...)
		default:
			// no escaping needed (printable ASCII)
			if b >= 0x20 && b <= 0x7E {
				sb = append(sb, b)
				continue l
			}
			// anything else is \x
			sb = append(sb, `\x`...)
			sb = append(sb, lowerhex[byte(b)>>4])
			sb = append(sb, lowerhex[byte(b)&0xF])
			continue l
		}
	}

	sb = append(sb, `"`...)
	return string(sb)
}
