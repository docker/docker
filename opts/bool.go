package opts

import (
	"errors"

	"github.com/spf13/pflag"
)

// BoolPtr returns a flag type that uses a bool pointer for a tri-value state.
func BoolPtr(v *bool) pflag.Value {
	return &boolPtr{v}
}

type boolPtr struct {
	v *bool
}

func (b *boolPtr) Set(value string) error {
	switch value {
	case "":
		return nil
	case "true":
		v := true
		b.v = &v
	case "false":
		v := false
		b.v = &v
	default:
		return errors.New("invalid value for bool flag")
	}
	return nil
}

func (b *boolPtr) String() string {
	if b.v == nil {
		return ""
	}
	if *b.v {
		return "true"
	}
	return "false"
}

func (b *boolPtr) Type() string {
	return "bool"
}

func (b *boolPtr) IsBool() bool {
	return true
}
