package httphead

import (
	"bytes"
	"sort"
)

// Option represents a header option.
type Option struct {
	Name       []byte
	Parameters Parameters
}

// Size returns number of bytes need to be allocated for use in opt.Copy.
func (opt Option) Size() int {
	return len(opt.Name) + opt.Parameters.bytes
}

// Copy copies all underlying []byte slices into p and returns new Option.
// Note that p must be at least of opt.Size() length.
func (opt Option) Copy(p []byte) Option {
	n := copy(p, opt.Name)
	opt.Name = p[:n]
	opt.Parameters, p = opt.Parameters.Copy(p[n:])
	return opt
}

// Clone is a shorthand for making slice of opt.Size() sequenced with Copy()
// call.
func (opt Option) Clone() Option {
	return opt.Copy(make([]byte, opt.Size()))
}

// String represents option as a string.
func (opt Option) String() string {
	return "{" + string(opt.Name) + " " + opt.Parameters.String() + "}"
}

// NewOption creates named option with given parameters.
func NewOption(name string, params map[string]string) Option {
	p := Parameters{}
	for k, v := range params {
		p.Set([]byte(k), []byte(v))
	}
	return Option{
		Name:       []byte(name),
		Parameters: p,
	}
}

// Equal reports whether option is equal to b.
func (opt Option) Equal(b Option) bool {
	if bytes.Equal(opt.Name, b.Name) {
		return opt.Parameters.Equal(b.Parameters)
	}
	return false
}

// Parameters represents option's parameters.
type Parameters struct {
	pos   int
	bytes int
	arr   [8]pair
	dyn   []pair
}

// Equal reports whether a equal to b.
func (p Parameters) Equal(b Parameters) bool {
	switch {
	case p.dyn == nil && b.dyn == nil:
	case p.dyn != nil && b.dyn != nil:
	default:
		return false
	}

	ad, bd := p.data(), b.data()
	if len(ad) != len(bd) {
		return false
	}

	sort.Sort(pairs(ad))
	sort.Sort(pairs(bd))

	for i := 0; i < len(ad); i++ {
		av, bv := ad[i], bd[i]
		if !bytes.Equal(av.key, bv.key) || !bytes.Equal(av.value, bv.value) {
			return false
		}
	}
	return true
}

// Size returns number of bytes that needed to copy p.
func (p *Parameters) Size() int {
	return p.bytes
}

// Copy copies all underlying []byte slices into dst and returns new
// Parameters.
// Note that dst must be at least of p.Size() length.
func (p *Parameters) Copy(dst []byte) (Parameters, []byte) {
	ret := Parameters{
		pos:   p.pos,
		bytes: p.bytes,
	}
	if p.dyn != nil {
		ret.dyn = make([]pair, len(p.dyn))
		for i, v := range p.dyn {
			ret.dyn[i], dst = v.copy(dst)
		}
	} else {
		for i, p := range p.arr {
			ret.arr[i], dst = p.copy(dst)
		}
	}
	return ret, dst
}

// Get returns value by key and flag about existence such value.
func (p *Parameters) Get(key string) (value []byte, ok bool) {
	for _, v := range p.data() {
		if string(v.key) == key {
			return v.value, true
		}
	}
	return nil, false
}

// Set sets value by key.
func (p *Parameters) Set(key, value []byte) {
	p.bytes += len(key) + len(value)

	if p.pos < len(p.arr) {
		p.arr[p.pos] = pair{key, value}
		p.pos++
		return
	}

	if p.dyn == nil {
		p.dyn = make([]pair, len(p.arr), len(p.arr)+1)
		copy(p.dyn, p.arr[:])
	}
	p.dyn = append(p.dyn, pair{key, value})
}

// ForEach iterates over parameters key-value pairs and calls cb for each one.
func (p *Parameters) ForEach(cb func(k, v []byte) bool) {
	for _, v := range p.data() {
		if !cb(v.key, v.value) {
			break
		}
	}
}

// String represents parameters as a string.
func (p *Parameters) String() (ret string) {
	ret = "["
	for i, v := range p.data() {
		if i > 0 {
			ret += " "
		}
		ret += string(v.key) + ":" + string(v.value)
	}
	return ret + "]"
}

func (p *Parameters) data() []pair {
	if p.dyn != nil {
		return p.dyn
	}
	return p.arr[:p.pos]
}

type pair struct {
	key, value []byte
}

func (p pair) copy(dst []byte) (pair, []byte) {
	n := copy(dst, p.key)
	p.key = dst[:n]
	m := n + copy(dst[n:], p.value)
	p.value = dst[n:m]

	dst = dst[m:]

	return p, dst
}

type pairs []pair

func (p pairs) Len() int           { return len(p) }
func (p pairs) Less(a, b int) bool { return bytes.Compare(p[a].key, p[b].key) == -1 }
func (p pairs) Swap(a, b int)      { p[a], p[b] = p[b], p[a] }
