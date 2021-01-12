// Package httphead contains utils for parsing HTTP and HTTP-grammar compatible
// text protocols headers.
//
// That is, this package first aim is to bring ability to easily parse
// constructions, described here https://tools.ietf.org/html/rfc2616#section-2
package httphead

import (
	"bytes"
	"strings"
)

// ScanTokens parses data in this form:
//
// list = 1#token
//
// It returns false if data is malformed.
func ScanTokens(data []byte, it func([]byte) bool) bool {
	lexer := &Scanner{data: data}

	var ok bool
	for lexer.Next() {
		switch lexer.Type() {
		case ItemToken:
			ok = true
			if !it(lexer.Bytes()) {
				return true
			}
		case ItemSeparator:
			if !isComma(lexer.Bytes()) {
				return false
			}
		default:
			return false
		}
	}

	return ok && !lexer.err
}

// ParseOptions parses all header options and appends it to given slice of
// Option. It returns flag of successful (wellformed input) parsing.
//
// Note that appended options are all consist of subslices of data. That is,
// mutation of data will mutate appended options.
func ParseOptions(data []byte, options []Option) ([]Option, bool) {
	var i int
	index := -1
	return options, ScanOptions(data, func(idx int, name, attr, val []byte) Control {
		if idx != index {
			index = idx
			i = len(options)
			options = append(options, Option{Name: name})
		}
		if attr != nil {
			options[i].Parameters.Set(attr, val)
		}
		return ControlContinue
	})
}

// SelectFlag encodes way of options selection.
type SelectFlag byte

// String represetns flag as string.
func (f SelectFlag) String() string {
	var flags [2]string
	var n int
	if f&SelectCopy != 0 {
		flags[n] = "copy"
		n++
	}
	if f&SelectUnique != 0 {
		flags[n] = "unique"
		n++
	}
	return "[" + strings.Join(flags[:n], "|") + "]"
}

const (
	// SelectCopy causes selector to copy selected option before appending it
	// to resulting slice.
	// If SelectCopy flag is not passed to selector, then appended options will
	// contain sub-slices of the initial data.
	SelectCopy SelectFlag = 1 << iota

	// SelectUnique causes selector to append only not yet existing option to
	// resulting slice. Unique is checked by comparing option names.
	SelectUnique
)

// OptionSelector contains configuration for selecting Options from header value.
type OptionSelector struct {
	// Check is a filter function that applied to every Option that possibly
	// could be selected.
	// If Check is nil all options will be selected.
	Check func(Option) bool

	// Flags contains flags for options selection.
	Flags SelectFlag

	// Alloc used to allocate slice of bytes when selector is configured with
	// SelectCopy flag. It will be called with number of bytes needed for copy
	// of single Option.
	// If Alloc is nil make is used.
	Alloc func(n int) []byte
}

// Select parses header data and appends it to given slice of Option.
// It also returns flag of successful (wellformed input) parsing.
func (s OptionSelector) Select(data []byte, options []Option) ([]Option, bool) {
	var current Option
	var has bool
	index := -1

	alloc := s.Alloc
	if alloc == nil {
		alloc = defaultAlloc
	}
	check := s.Check
	if check == nil {
		check = defaultCheck
	}

	ok := ScanOptions(data, func(idx int, name, attr, val []byte) Control {
		if idx != index {
			if has && check(current) {
				if s.Flags&SelectCopy != 0 {
					current = current.Copy(alloc(current.Size()))
				}
				options = append(options, current)
				has = false
			}
			if s.Flags&SelectUnique != 0 {
				for i := len(options) - 1; i >= 0; i-- {
					if bytes.Equal(options[i].Name, name) {
						return ControlSkip
					}
				}
			}
			index = idx
			current = Option{Name: name}
			has = true
		}
		if attr != nil {
			current.Parameters.Set(attr, val)
		}

		return ControlContinue
	})
	if has && check(current) {
		if s.Flags&SelectCopy != 0 {
			current = current.Copy(alloc(current.Size()))
		}
		options = append(options, current)
	}

	return options, ok
}

func defaultAlloc(n int) []byte { return make([]byte, n) }
func defaultCheck(Option) bool  { return true }

// Control represents operation that scanner should perform.
type Control byte

const (
	// ControlContinue causes scanner to continue scan tokens.
	ControlContinue Control = iota
	// ControlBreak causes scanner to stop scan tokens.
	ControlBreak
	// ControlSkip causes scanner to skip current entity.
	ControlSkip
)

// ScanOptions parses data in this form:
//
// values = 1#value
// value = token *( ";" param )
// param = token [ "=" (token | quoted-string) ]
//
// It calls given callback with the index of the option, option itself and its
// parameter (attribute and its value, both could be nil). Index is useful when
// header contains multiple choises for the same named option.
//
// Given callback should return one of the defined Control* values.
// ControlSkip means that passed key is not in caller's interest. That is, all
// parameters of that key will be skipped.
// ControlBreak means that no more keys and parameters should be parsed. That
// is, it must break parsing immediately.
// ControlContinue means that caller want to receive next parameter and its
// value or the next key.
//
// It returns false if data is malformed.
func ScanOptions(data []byte, it func(index int, option, attribute, value []byte) Control) bool {
	lexer := &Scanner{data: data}

	var ok bool
	var state int
	const (
		stateKey = iota
		stateParamBeforeName
		stateParamName
		stateParamBeforeValue
		stateParamValue
	)

	var (
		index             int
		key, param, value []byte
		mustCall          bool
	)
	for lexer.Next() {
		var (
			call      bool
			growIndex int
		)

		t := lexer.Type()
		v := lexer.Bytes()

		switch t {
		case ItemToken:
			switch state {
			case stateKey, stateParamBeforeName:
				key = v
				state = stateParamBeforeName
				mustCall = true
			case stateParamName:
				param = v
				state = stateParamBeforeValue
				mustCall = true
			case stateParamValue:
				value = v
				state = stateParamBeforeName
				call = true
			default:
				return false
			}

		case ItemString:
			if state != stateParamValue {
				return false
			}
			value = v
			state = stateParamBeforeName
			call = true

		case ItemSeparator:
			switch {
			case isComma(v) && state == stateKey:
				// Nothing to do.

			case isComma(v) && state == stateParamBeforeName:
				state = stateKey
				// Make call only if we have not called this key yet.
				call = mustCall
				if !call {
					// If we have already called callback with the key
					// that just ended.
					index++
				} else {
					// Else grow the index after calling callback.
					growIndex = 1
				}

			case isComma(v) && state == stateParamBeforeValue:
				state = stateKey
				growIndex = 1
				call = true

			case isSemicolon(v) && state == stateParamBeforeName:
				state = stateParamName

			case isSemicolon(v) && state == stateParamBeforeValue:
				state = stateParamName
				call = true

			case isEquality(v) && state == stateParamBeforeValue:
				state = stateParamValue

			default:
				return false
			}

		default:
			return false
		}

		if call {
			switch it(index, key, param, value) {
			case ControlBreak:
				// User want to stop to parsing parameters.
				return true

			case ControlSkip:
				// User want to skip current param.
				state = stateKey
				lexer.SkipEscaped(',')

			case ControlContinue:
				// User is interested in rest of parameters.
				// Nothing to do.

			default:
				panic("unexpected control value")
			}
			ok = true
			param = nil
			value = nil
			mustCall = false
			index += growIndex
		}
	}
	if mustCall {
		ok = true
		it(index, key, param, value)
	}

	return ok && !lexer.err
}

func isComma(b []byte) bool {
	return len(b) == 1 && b[0] == ','
}
func isSemicolon(b []byte) bool {
	return len(b) == 1 && b[0] == ';'
}
func isEquality(b []byte) bool {
	return len(b) == 1 && b[0] == '='
}
