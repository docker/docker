package httpbinding

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/document"
	httpbinding "github.com/aws/smithy-go/encoding/httpbinding"
	"github.com/aws/smithy-go/traits"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	awsjson "github.com/aws/smithy-go/transport/http/protocol/internal/json"
)

// ShapeSerializer routes top-level input struct members to their HTTP binding
// locations. Members without HTTP binding traits delegate to an inner
// serializer for whatever protocol is being used.
//
// ShapeSerializer adds some API surface on top of the normal
// smithy.ShapeSerializer. Specifically it adds [ShapeSerializer.Build] for
// handing REST-protocol payloads, since the actual source of the payload is
// going to vary on a per-operation basis and isn't known until the input's
// Serialize is called. The caller (so, the protocol implementation) should
// set the HTTP request body by calling Build after Serialize.
type ShapeSerializer struct {
	request *smithyhttp.Request
	encoder *httpbinding.Encoder
	input   smithy.ShapeSerializer
	options ShapeSerializerOptions

	// set when an input member is bound via @httpPayload
	httpPayload            []byte
	httpPayloadContentType string

	// set when an input blob is bound via @httpPayload + @streaming
	streamingContentType string

	mapMode    mapBindingMode
	mapPrefix  string
	currentKey string

	listMode     listBindingMode
	listName     string
	listHasItems bool

	noBody           bool
	hasStructPayload bool

	depth int
}

// ShapeSerializerOptions configures a ShapeSerializer.
type ShapeSerializerOptions struct{}

// NewShapeSerializer creates a ShapeSerializer for the given operation schema
// and request. It handles the initial setup from use of an
// httpbinding.Encoder.
func NewShapeSerializer(op *smithy.Schema, req *smithyhttp.Request, in smithy.ShapeSerializer, opts ...func(*ShapeSerializerOptions)) (*ShapeSerializer, error) {
	httpTrait, ok := smithy.SchemaTrait[*traits.HTTP](op)
	if !ok {
		return nil, fmt.Errorf("no @http trait on op schema")
	}

	req.Method = httpTrait.Method
	path, query := httpbinding.SplitURI(httpTrait.URI)
	enc, err := httpbinding.NewEncoder(path, query, req.Header)
	if err != nil {
		return nil, fmt.Errorf("new encoder: %w", err)
	}

	return &ShapeSerializer{
		request: req,
		encoder: enc,
		input:   in,
	}, nil
}

// Build encodes HTTP binding values into the request and sets the request
// body. The defaultContentType is used for the protocol body (e.g.
// "application/json") when no explicit payload is present.
//
// The body is resolved in the following priority:
//  1. Streaming payload (input implements StreamingInput with non-nil stream)
//  2. Raw payload bytes (blob/string member with @httpPayload)
//  3. Serialized protocol body (e.g. JSON)
//  4. Empty struct payload (struct member with @httpPayload, sends "{}")
func (s *ShapeSerializer) Build(in smithy.Serializable, defaultContentType string) error {
	req := s.request

	built, err := s.encoder.Encode(req.Request)
	if err != nil {
		return fmt.Errorf("encode httpbinding: %w", err)
	}
	req.Request = built

	// (1) streaming payload
	if si, ok := in.(smithy.StreamingInput); ok && si.GetPayloadStream() != nil {
		contentType := s.streamingContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		return s.setBody(si.GetPayloadStream(), contentType)
	}

	var payload []byte
	var contentType string

	// (2) explicit @httpPayload (blob/string)
	if s.httpPayload != nil {
		payload = s.httpPayload
		contentType = s.httpPayloadContentType
	} else { // (3) protocol body
		payload = s.input.Bytes()
		contentType = defaultContentType
	}

	// (4) empty struct @httpPayload
	if len(payload) == 0 && s.hasStructPayload {
		payload = []byte("{}")
		contentType = defaultContentType
	}

	if len(payload) == 0 {
		return nil
	}
	return s.setBody(bytes.NewReader(payload), contentType)
}

func (s *ShapeSerializer) setBody(body io.Reader, contentType string) error {
	if s.request.Header.Get("Content-Type") == "" {
		s.request.Header.Set("Content-Type", contentType)
	}
	sreq, err := s.request.SetStream(body)
	if err != nil {
		return fmt.Errorf("set stream: %w", err)
	}
	*s.request = *sreq
	return nil
}

type mapBindingMode int

const (
	mapModeNone mapBindingMode = iota
	mapModePrefixHeaders
	mapModeQueryParams
)

type listBindingMode int

const (
	listModeNone listBindingMode = iota
	listModeHeader
	listModeQuery
)

type bind int

const (
	bindBody bind = iota
	bindHeader
	bindHeaderList
	bindQuery
	bindQueryList
	bindLabel
)

func (s *ShapeSerializer) resolveBinding(schema *smithy.Schema) (bind, string) {
	if s.listMode == listModeHeader {
		return bindHeaderList, s.listName
	}
	if s.listMode == listModeQuery {
		return bindQueryList, s.listName
	}
	if name, ok := isHTTPHeader(schema); ok {
		return bindHeader, name
	}
	if isHTTPLabel(schema) {
		return bindLabel, schema.MemberName()
	}
	if q, ok := isHTTPQuery(schema); ok {
		return bindQuery, q.Name
	}
	return bindBody, ""
}

var _ smithy.ShapeSerializer = (*ShapeSerializer)(nil)

// Bytes returns the serialized body bytes.
func (s *ShapeSerializer) Bytes() []byte {
	return s.input.Bytes()
}

func isHTTPHeader(schema *smithy.Schema) (string, bool) {
	h, ok := smithy.SchemaTrait[*traits.HTTPHeader](schema)
	if !ok {
		return "", false
	}
	return http.CanonicalHeaderKey(h.Name), true
}

func isHTTPLabel(schema *smithy.Schema) bool {
	_, ok := smithy.SchemaTrait[*traits.HTTPLabel](schema)
	return ok
}

func isHTTPQuery(schema *smithy.Schema) (*traits.HTTPQuery, bool) {
	return smithy.SchemaTrait[*traits.HTTPQuery](schema)
}

func isHTTPPayload(schema *smithy.Schema) bool {
	_, ok := smithy.SchemaTrait[*traits.HTTPPayload](schema)
	return ok
}

func isHTTPPrefixHeaders(schema *smithy.Schema) (*traits.HTTPPrefixHeaders, bool) {
	ph, ok := smithy.SchemaTrait[*traits.HTTPPrefixHeaders](schema)
	return ph, ok
}

func isHTTPQueryParams(schema *smithy.Schema) bool {
	_, ok := smithy.SchemaTrait[*traits.HTTPQueryParams](schema)
	return ok
}

func hasBodyMembers(schema *smithy.Schema) bool {
	for _, member := range schema.Members() {
		if !isHTTPBound(member) {
			return true
		}
	}
	return false
}

func isHTTPBound(schema *smithy.Schema) bool {
	if _, ok := isHTTPHeader(schema); ok {
		return true
	}
	if _, ok := isHTTPPrefixHeaders(schema); ok {
		return true
	}
	if isHTTPLabel(schema) {
		return true
	}
	if _, ok := isHTTPQuery(schema); ok {
		return true
	}
	if isHTTPQueryParams(schema) {
		return true
	}
	if isHTTPPayload(schema) {
		return true
	}
	return false
}

// WriteString implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteString(schema *smithy.Schema, v string) {
	switch s.mapMode {
	case mapModePrefixHeaders:
		s.encoder.SetHeader(http.CanonicalHeaderKey(s.mapPrefix + s.currentKey)).String(v)
		return
	case mapModeQueryParams:
		s.encoder.AddQuery(s.currentKey).String(v)
		return
	}

	bt, bn := s.resolveBinding(schema)
	switch bt {
	case bindHeaderList:
		escaped := v
		if strings.ContainsAny(v, ",\"") {
			escaped = strconv.Quote(v)
		}
		s.encoder.AddHeader(bn).String(escaped)
		s.listHasItems = true
	case bindQueryList:
		s.encoder.AddQuery(bn).String(v)
	case bindHeader:
		if _, ok := smithy.SchemaTrait[*traits.MediaType](schema); ok {
			s.encoder.SetHeader(bn).String(base64.StdEncoding.EncodeToString([]byte(v)))
			return
		}
		s.encoder.SetHeader(bn).String(v)
	case bindLabel:
		s.encoder.SetURI(bn).String(v)
	case bindQuery:
		s.encoder.SetQuery(bn).String(v)
	default:
		if isHTTPPayload(schema) {
			s.httpPayload = []byte(v)
			s.httpPayloadContentType = "text/plain"
			return
		}
		s.input.WriteString(schema, v)
	}
}

// WriteBool implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteBool(schema *smithy.Schema, v bool) {
	bt, bn := s.resolveBinding(schema)
	switch bt {
	case bindHeaderList:
		s.encoder.AddHeader(bn).Boolean(v)
		s.listHasItems = true
	case bindQueryList:
		s.encoder.AddQuery(bn).Boolean(v)
	case bindHeader:
		s.encoder.SetHeader(bn).Boolean(v)
	case bindLabel:
		s.encoder.SetURI(bn).Boolean(v)
	case bindQuery:
		s.encoder.SetQuery(bn).Boolean(v)
	default:
		s.input.WriteBool(schema, v)
	}
}

// WriteInt8 implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteInt8(schema *smithy.Schema, v int8) {
	bt, bn := s.resolveBinding(schema)
	switch bt {
	case bindHeaderList:
		s.encoder.AddHeader(bn).Byte(v)
		s.listHasItems = true
	case bindQueryList:
		s.encoder.AddQuery(bn).Byte(v)
	case bindHeader:
		s.encoder.SetHeader(bn).Byte(v)
	case bindLabel:
		s.encoder.SetURI(bn).Byte(v)
	case bindQuery:
		s.encoder.SetQuery(bn).Byte(v)
	default:
		s.input.WriteInt8(schema, v)
	}
}

// WriteInt16 implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteInt16(schema *smithy.Schema, v int16) {
	bt, bn := s.resolveBinding(schema)
	switch bt {
	case bindHeaderList:
		s.encoder.AddHeader(bn).Short(v)
		s.listHasItems = true
	case bindQueryList:
		s.encoder.AddQuery(bn).Short(v)
	case bindHeader:
		s.encoder.SetHeader(bn).Short(v)
	case bindLabel:
		s.encoder.SetURI(bn).Short(v)
	case bindQuery:
		s.encoder.SetQuery(bn).Short(v)
	default:
		s.input.WriteInt16(schema, v)
	}
}

// WriteInt32 implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteInt32(schema *smithy.Schema, v int32) {
	bt, bn := s.resolveBinding(schema)
	switch bt {
	case bindHeaderList:
		s.encoder.AddHeader(bn).Integer(v)
		s.listHasItems = true
	case bindQueryList:
		s.encoder.AddQuery(bn).Integer(v)
	case bindHeader:
		s.encoder.SetHeader(bn).Integer(v)
	case bindLabel:
		s.encoder.SetURI(bn).Integer(v)
	case bindQuery:
		s.encoder.SetQuery(bn).Integer(v)
	default:
		s.input.WriteInt32(schema, v)
	}
}

// WriteInt64 implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteInt64(schema *smithy.Schema, v int64) {
	bt, bn := s.resolveBinding(schema)
	switch bt {
	case bindHeaderList:
		s.encoder.AddHeader(bn).Long(v)
		s.listHasItems = true
	case bindQueryList:
		s.encoder.AddQuery(bn).Long(v)
	case bindHeader:
		s.encoder.SetHeader(bn).Long(v)
	case bindLabel:
		s.encoder.SetURI(bn).Long(v)
	case bindQuery:
		s.encoder.SetQuery(bn).Long(v)
	default:
		s.input.WriteInt64(schema, v)
	}
}

// WriteFloat32 implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteFloat32(schema *smithy.Schema, v float32) {
	bt, bn := s.resolveBinding(schema)
	switch bt {
	case bindHeaderList:
		s.encoder.AddHeader(bn).Float(v)
		s.listHasItems = true
	case bindQueryList:
		s.encoder.AddQuery(bn).Float(v)
	case bindHeader:
		s.encoder.SetHeader(bn).Float(v)
	case bindLabel:
		s.encoder.SetURI(bn).Float(v)
	case bindQuery:
		s.encoder.SetQuery(bn).Float(v)
	default:
		s.input.WriteFloat32(schema, v)
	}
}

// WriteFloat64 implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteFloat64(schema *smithy.Schema, v float64) {
	bt, bn := s.resolveBinding(schema)
	switch bt {
	case bindHeaderList:
		s.encoder.AddHeader(bn).Double(v)
		s.listHasItems = true
	case bindQueryList:
		s.encoder.AddQuery(bn).Double(v)
	case bindHeader:
		s.encoder.SetHeader(bn).Double(v)
	case bindLabel:
		s.encoder.SetURI(bn).Double(v)
	case bindQuery:
		s.encoder.SetQuery(bn).Double(v)
	default:
		s.input.WriteFloat64(schema, v)
	}
}

// WriteBlob implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteBlob(schema *smithy.Schema, v []byte) {
	if isHTTPPayload(schema) {
		s.httpPayload = v
		if mt, ok := smithy.SchemaTrait[*traits.MediaType](schema); ok {
			s.httpPayloadContentType = mt.Type
		} else {
			s.httpPayloadContentType = "application/octet-stream"
		}
		return
	}
	if name, ok := isHTTPHeader(schema); ok {
		s.encoder.SetHeader(name).Blob(v)
		return
	}
	s.input.WriteBlob(schema, v)
}

// WriteTime implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteTime(schema *smithy.Schema, v time.Time) {
	bt, bn := s.resolveBinding(schema)
	switch bt {
	case bindHeaderList:
		s.encoder.AddHeader(bn).String(formatTimestamp(schema, "http-date", v))
		s.listHasItems = true
	case bindQueryList:
		s.encoder.AddQuery(bn).String(formatTimestamp(schema, "date-time", v))
	case bindHeader:
		s.encoder.SetHeader(bn).String(formatTimestamp(schema, "http-date", v))
	case bindLabel:
		s.encoder.SetURI(bn).String(formatTimestamp(schema, "date-time", v))
	case bindQuery:
		s.encoder.SetQuery(bn).String(formatTimestamp(schema, "date-time", v))
	default:
		s.input.WriteTime(schema, v)
	}
}

// WriteList implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteList(schema *smithy.Schema) {
	if s.mapMode == mapModeQueryParams {
		s.listMode = listModeQuery
		s.listName = s.currentKey
		return
	}
	if name, ok := isHTTPHeader(schema); ok {
		s.listMode = listModeHeader
		s.listName = name
		s.listHasItems = false
		return
	}
	if q, ok := isHTTPQuery(schema); ok {
		s.listMode = listModeQuery
		s.listName = q.Name
		s.listHasItems = false
		return
	}
	s.input.WriteList(schema)
}

// CloseList implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) CloseList() {
	if s.listMode != listModeNone {
		if !s.listHasItems && s.listMode == listModeHeader {
			s.encoder.SetHeader(s.listName).String("")
		}
		s.listMode = listModeNone
		s.listName = ""
		s.listHasItems = false
		return
	}
	s.input.CloseList()
}

// WriteMap implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteMap(schema *smithy.Schema) {
	if ph, ok := isHTTPPrefixHeaders(schema); ok {
		s.mapMode = mapModePrefixHeaders
		s.mapPrefix = ph.Prefix
		return
	}

	if isHTTPQueryParams(schema) {
		s.mapMode = mapModeQueryParams
		return
	}

	s.input.WriteMap(schema)
}

// WriteKey implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteKey(schema *smithy.Schema, key string) {
	switch s.mapMode {
	case mapModePrefixHeaders, mapModeQueryParams:
		s.currentKey = key
	default:
		s.input.WriteKey(schema, key)
	}
}

// CloseMap implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) CloseMap() {
	if s.mapMode != mapModeNone {
		s.mapMode = mapModeNone
		s.mapPrefix = ""
		s.currentKey = ""
		return
	}
	s.input.CloseMap()
}

// WriteStruct implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteStruct(schema *smithy.Schema) {
	s.depth++

	if s.depth > 1 { // not the root, HTTP bindings don't apply
		if isHTTPPayload(schema) {
			s.hasStructPayload = true
		}
		s.input.WriteStruct(schema)
		return
	}

	for _, m := range schema.Members() {
		if !isHTTPPayload(m) {
			continue
		}

		if _, ok := smithy.SchemaTrait[*traits.Streaming](m); ok {
			s.streamingContentType = "application/octet-stream"
			if mt, ok := smithy.SchemaTrait[*traits.MediaType](m); ok {
				s.streamingContentType = mt.Type
			}
		} else if m.Type() == smithy.ShapeTypeStructure {
			s.hasStructPayload = true
		}
	}

	if !hasBodyMembers(schema) {
		s.noBody = true
		return
	}

	// IMPLICIT payload, ie. no @httpPayload but there is at least one member
	// that isn't http-bound
	s.input.WriteStruct(schema)
}

// CloseStruct implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) CloseStruct() {
	s.depth--

	// we skip the close if there was no payload to begin with, implicit or
	// otherwise
	if s.depth > 0 || !s.noBody {
		s.input.CloseStruct()
	}
}

// WriteUnion implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteUnion(schema, variant *smithy.Schema) {
	s.input.WriteUnion(schema, variant)
}

// CloseUnion implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) CloseUnion() {
	s.input.CloseUnion()
}

// WriteNil implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteNil(schema *smithy.Schema) {
	s.input.WriteNil(schema)
}

// WriteBigInt implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteBigInt(schema *smithy.Schema, v *big.Int) {
	s.input.WriteBigInt(schema, v)
}

// WriteBigFloat implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteBigFloat(schema *smithy.Schema, v *big.Float) {
	s.input.WriteBigFloat(schema, v)
}

// WriteDocument implements [smithy.ShapeSerializer].
func (s *ShapeSerializer) WriteDocument(schema *smithy.Schema, v document.Value) {
	if isHTTPPayload(schema) {
		// httpPayload document: serialize to raw bytes for the body.
		doc := awsjson.NewShapeSerializer()
		doc.WriteDocument(schema, v)
		s.httpPayload = doc.Bytes()
		s.httpPayloadContentType = "application/json"
		return
	}
	s.input.WriteDocument(schema, v)
}
