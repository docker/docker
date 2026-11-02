package httpbinding

import (
	"encoding/base64"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/document"
	"github.com/aws/smithy-go/traits"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// ShapeDeserializer reads HTTP-bound output struct members from the response,
// delegates body members to the wrapped body deserializer.
type ShapeDeserializer struct {
	response *http.Response
	body     smithy.ShapeDeserializer
	payload  []byte

	// ALL http binding traits are applied on the top-level output struct, for
	// anything nested we are just delegating to the payload deserializer
	depth    int
	topLevel *smithy.Schema

	// unlike an RPC-style protocol, members of the top-level output could be
	// HTTP-bound, so we track that when ReadStruct is first called and "yield"
	// them back to the caller through ReadStructMember
	currentBinding *smithy.Schema
	bindings       []*smithy.Schema
	bindingIndex   int

	inBody     bool
	hasPayload bool

	inHeaderList     bool
	headerListValues []string
	headerListIdx    int

	inPrefixMap  bool
	prefixValue  string
	prefixKeys   []string
	prefixKeyIdx int
}

var _ smithy.ShapeDeserializer = (*ShapeDeserializer)(nil)

// ShapeDeserializerOptions configures ShapeDeserializer.
type ShapeDeserializerOptions struct{}

// NewShapeDeserializer creates a ShapeDeserializer for the given HTTP
// response.
//
// The payload should be nil in streaming-blob response operations.
func NewShapeDeserializer(resp *http.Response, body smithy.ShapeDeserializer, payload []byte, opts ...func(*ShapeDeserializerOptions)) *ShapeDeserializer {
	return &ShapeDeserializer{
		response: resp,
		body:     body,
		payload:  payload,
	}
}

// ReadStruct implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadStruct(s *smithy.Schema) error {
	d.depth++
	if d.depth > 1 {
		return d.body.ReadStruct(s)
	}

	d.topLevel = s
	for _, member := range s.Members() {
		if _, ok := smithy.SchemaTrait[*traits.HTTPPayload](member); ok {
			d.hasPayload = true
		}
		if d.isBindingSet(member) {
			d.bindings = append(d.bindings, member)
		}
	}

	return nil
}

// ReadStructMember implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadStructMember() (*smithy.Schema, error) {
	if d.depth > 1 {
		ms, err := d.body.ReadStructMember()
		if ms == nil {
			d.depth--
		}
		return ms, err
	}

	if d.bindingIndex < len(d.bindings) {
		member := d.bindings[d.bindingIndex]
		d.bindingIndex++
		d.currentBinding = member
		return member, nil
	}
	d.currentBinding = nil

	if d.hasPayload { // i.e. no unbound members
		d.depth--
		return nil, nil
	}

	if !d.inBody {
		d.inBody = true
		if err := d.body.ReadStruct(d.topLevel); err != nil {
			return nil, err
		}
	}

	ms, err := d.body.ReadStructMember()
	if ms == nil {
		d.depth--
	}
	return ms, err
}

// ReadString implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadString(s *smithy.Schema, v *string) error {
	if d.inHeaderList {
		*v = d.headerListValues[d.headerListIdx]
		d.headerListIdx++
		return nil
	}
	if d.isCurrentBinding(s) {
		if _, ok := isHTTPHeader(s); ok {
			hv, err := d.readHeaderString(s)
			if err != nil {
				return err
			}
			*v = hv
			return nil
		}
		if _, ok := smithy.SchemaTrait[*traits.HTTPPayload](s); ok {
			*v = string(d.payload)
			return nil
		}
	}
	if d.inPrefixMap {
		key := d.prefixKeys[d.prefixKeyIdx-1]
		*v = d.response.Header.Get(d.prefixValue + key)
		return nil
	}
	return d.body.ReadString(s, v)
}

// ReadBool implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadBool(s *smithy.Schema, v *bool) error {
	if !d.inHeaderList && !d.isCurrentBinding(s) {
		return d.body.ReadBool(s, v)
	}

	name, _ := isHTTPHeader(s)

	var hv string
	if d.inHeaderList {
		hv = d.nextHeaderValue()
	} else {
		hv = d.response.Header.Get(name)
	}

	n, err := strconv.ParseBool(hv)
	if err != nil {
		return err
	}

	*v = n
	return nil
}

// ReadInt8 implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadInt8(s *smithy.Schema, v *int8) error {
	if !d.inHeaderList && !d.isCurrentBinding(s) {
		return d.body.ReadInt8(s, v)
	}
	return readHeaderInt[int8](d, s, v)
}

// ReadInt16 implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadInt16(s *smithy.Schema, v *int16) error {
	if !d.inHeaderList && !d.isCurrentBinding(s) {
		return d.body.ReadInt16(s, v)
	}
	return readHeaderInt[int16](d, s, v)
}

// ReadInt32 implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadInt32(s *smithy.Schema, v *int32) error {
	if !d.inHeaderList && !d.isCurrentBinding(s) {
		return d.body.ReadInt32(s, v)
	}

	// https://smithy.io/2.0/spec/http-bindings.html#httpresponsecode-trait
	//
	// The httpResponseCode trait can be applied to structure members that
	// target an integer within any structure that has no input trait applied.
	if _, ok := smithy.SchemaTrait[*traits.HTTPResponseCode](s); ok {
		*v = int32(d.response.StatusCode)
		return nil
	}

	return readHeaderInt[int32](d, s, v)
}

// ReadInt64 implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadInt64(s *smithy.Schema, v *int64) error {
	if !d.inHeaderList && !d.isCurrentBinding(s) {
		return d.body.ReadInt64(s, v)
	}
	return readHeaderInt[int64](d, s, v)
}

// ReadFloat32 implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadFloat32(s *smithy.Schema, v *float32) error {
	if !d.inHeaderList && !d.isCurrentBinding(s) {
		return d.body.ReadFloat32(s, v)
	}
	return readHeaderFloat[float32](d, s, v)
}

// ReadFloat64 implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadFloat64(s *smithy.Schema, v *float64) error {
	if !d.inHeaderList && !d.isCurrentBinding(s) {
		return d.body.ReadFloat64(s, v)
	}
	return readHeaderFloat[float64](d, s, v)
}

// ReadTime implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadTime(s *smithy.Schema, v *time.Time) error {
	if d.inHeaderList {
		return d.readHeaderListTime(func(t time.Time) { *v = t })
	}
	if d.isCurrentBinding(s) {
		t, err := d.readHeaderTime(s)
		if err != nil {
			return err
		}
		*v = t
		return nil
	}
	return d.body.ReadTime(s, v)
}

func (d *ShapeDeserializer) readHeaderTime(s *smithy.Schema) (time.Time, error) {
	name, _ := isHTTPHeader(s)
	hv := d.response.Header.Get(name)
	t, err := parseTimestamp(s, "http-date", hv)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

func (d *ShapeDeserializer) readHeaderListTime(assign func(time.Time)) error {
	hv := d.headerListValues[d.headerListIdx]
	d.headerListIdx++
	t, err := parseTimestamp(nil, "http-date", hv)
	if err != nil {
		return err
	}
	assign(t)
	return nil
}

// ReadBlob implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadBlob(s *smithy.Schema, v *[]byte) error {
	if !d.isCurrentBinding(s) {
		return d.body.ReadBlob(s, v)
	}

	if _, ok := smithy.SchemaTrait[*traits.HTTPPayload](s); ok {
		*v = d.payload
		return nil
	}

	name, ok := isHTTPHeader(s)
	if !ok {
		return fmt.Errorf("ReadBlob: unhandled http binding")
	}

	hv := d.response.Header.Get(name)
	b, err := base64.StdEncoding.DecodeString(hv)
	if err != nil {
		return err
	}
	*v = b
	return nil
}

// ReadList implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadList(s *smithy.Schema) error {
	if !d.isCurrentBinding(s) {
		return d.body.ReadList(s)
	}

	name, ok := isHTTPHeader(s)
	if !ok {
		return fmt.Errorf("ReadList called outside of payload / http binding")
	}

	d.inHeaderList = true
	d.headerListIdx = 0
	if s.ListMember() != nil && s.ListMember().Type() == smithy.ShapeTypeTimestamp && timestampFormat(s.ListMember(), "http-date") == "http-date" {
		vs, err := smithyhttp.SplitHTTPDateTimestampHeaderListValues(d.response.Header.Values(name))
		if err != nil {
			return err
		}

		d.headerListValues = vs
	} else {
		vs, err := smithyhttp.SplitHeaderListValues(d.response.Header.Values(name))
		if err != nil {
			return err
		}

		d.headerListValues = vs
	}
	return nil
}

// ReadListItem implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadListItem(s *smithy.Schema) (bool, error) {
	if !d.inHeaderList {
		return d.body.ReadListItem(s)
	}

	if d.headerListIdx >= len(d.headerListValues) {
		d.inHeaderList = false
		return false, nil
	}
	return true, nil
}

// ReadMap implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadMap(s *smithy.Schema) error {
	if !d.isCurrentBinding(s) {
		return d.body.ReadMap(s)
	}

	ph, ok := smithy.SchemaTrait[*traits.HTTPPrefixHeaders](s)
	if !ok {
		return fmt.Errorf("ReadMap called outside of payload / http binding")
	}

	d.inPrefixMap = true
	d.prefixValue = ph.Prefix
	d.prefixKeyIdx = 0

	canon := http.CanonicalHeaderKey(ph.Prefix)
	for name := range d.response.Header {
		if len(name) > len(canon) && strings.EqualFold(name[:len(canon)], canon) {
			d.prefixKeys = append(d.prefixKeys, strings.ToLower(name[len(canon):]))
		}
	}

	return nil
}

// ReadMapKey implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadMapKey(s *smithy.Schema) (string, bool, error) {
	if !d.inPrefixMap {
		return d.body.ReadMapKey(s)
	}

	if d.prefixKeyIdx >= len(d.prefixKeys) {
		d.inPrefixMap = false
		return "", false, nil
	}
	key := d.prefixKeys[d.prefixKeyIdx]
	d.prefixKeyIdx++
	return key, true, nil
}

// ReadNil implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadNil(s *smithy.Schema) (bool, error) {
	if d.inPrefixMap || d.inHeaderList {
		return false, nil
	}
	return d.body.ReadNil(s)
}

// ReadDocument implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadDocument(s *smithy.Schema, v *document.Value) error {
	return d.body.ReadDocument(s, v)
}

// ReadUnion implements [smithy.ShapeDeserializer].
func (d *ShapeDeserializer) ReadUnion(s *smithy.Schema) (*smithy.Schema, error) {
	return d.body.ReadUnion(s)
}

// whether the schema we're operating right now is the current HTTP binding
// that we gave back to the caller
func (d *ShapeDeserializer) isCurrentBinding(schema *smithy.Schema) bool {
	return schema != nil && schema == d.currentBinding
}

func (d *ShapeDeserializer) isBindingSet(schema *smithy.Schema) bool {
	if name, ok := isHTTPHeader(schema); ok {
		return len(d.response.Header.Values(name)) > 0
	}

	if trait, ok := isHTTPPrefixHeaders(schema); ok {
		canon := http.CanonicalHeaderKey(trait.Prefix)
		for name := range d.response.Header {
			if len(name) > len(canon) && strings.EqualFold(name[:len(canon)], canon) {
				return true
			}
		}
		return false
	}

	if _, ok := smithy.SchemaTrait[*traits.HTTPResponseCode](schema); ok {
		return true
	}

	if _, ok := smithy.SchemaTrait[*traits.HTTPPayload](schema); ok {
		return len(d.payload) > 0
	}

	return false
}

func (d *ShapeDeserializer) readHeaderString(s *smithy.Schema) (string, error) {
	name, _ := isHTTPHeader(s)

	hv := d.response.Header.Get(name)
	if _, ok := smithy.SchemaTrait[*traits.MediaType](s); ok {
		b, err := base64.StdEncoding.DecodeString(hv)
		if err != nil {
			return "", err
		}
		hv = string(b)
	}
	return hv, nil
}

func (d *ShapeDeserializer) nextHeaderValue() string {
	v := d.headerListValues[d.headerListIdx]
	d.headerListIdx++
	return v
}

type intn interface {
	int8 | int16 | int32 | int64
}

func readHeaderInt[T intn](d *ShapeDeserializer, s *smithy.Schema, v *T) error {
	name, _ := isHTTPHeader(s)

	var hv string
	if d.inHeaderList {
		hv = d.nextHeaderValue()
	} else {
		hv = d.response.Header.Get(name)
	}

	n, err := strconv.ParseInt(hv, 10, 64)
	if err != nil {
		return err
	}

	*v = T(n)
	return nil
}

type floatn interface {
	float32 | float64
}

func readHeaderFloat[T floatn](d *ShapeDeserializer, s *smithy.Schema, v *T) error {
	name, _ := isHTTPHeader(s)

	var hv string
	if d.inHeaderList {
		hv = d.nextHeaderValue()
	} else {
		hv = d.response.Header.Get(name)
	}

	n, err := strconv.ParseFloat(hv, 64)
	if err != nil {
		return err
	}

	*v = T(n)
	return nil
}

// ReadBigInt is unimplemented and will return an error.
func (d *ShapeDeserializer) ReadBigInt(_ *smithy.Schema, _ *big.Int) error {
	return fmt.Errorf("unimplemented")
}

// ReadBigFloat is unimplemented and will return an error.
func (d *ShapeDeserializer) ReadBigFloat(_ *smithy.Schema, _ *big.Float) error {
	return fmt.Errorf("unimplemented")
}

// HasBlobPayload reports whether the given output shape binds a blob member as
// @httpPayload. Such a payload is handed to the caller by reference, so the
// buffer holding it cannot be reused.
//
// The answer is cached per schema: this is called on every response, and walking
// members with a trait lookup each is not free at that rate.
func HasBlobPayload(output *smithy.Schema) bool {
	if output == nil {
		return false
	}
	return getExt(output).hasBlobPayload
}
