package restjson1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/aws/smithy-go"
	internales "github.com/aws/smithy-go/internal/eventstream"
	internalserde "github.com/aws/smithy-go/internal/serde"
	internalsync "github.com/aws/smithy-go/internal/sync"
	smithyio "github.com/aws/smithy-go/io"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	internalhttpbinding "github.com/aws/smithy-go/transport/http/protocol/internal/httpbinding"
	internaljson "github.com/aws/smithy-go/transport/http/protocol/internal/json"
)

// ProtocolOptions configures aws.protocols#restJson1.
type ProtocolOptions struct{}

// New returns an instance of the aws.protocols#restJson1 protocol.
func New(_ *smithy.ServiceSchema, opts ...func(*ProtocolOptions)) *Protocol {
	var o ProtocolOptions
	for _, fn := range opts {
		fn(&o)
	}
	return &Protocol{
		bufs: internalsync.NewBufferPool(),
		eventstream: &internales.Codec{
			Serializer:   func() smithy.ShapeSerializer { return internaljson.NewShapeSerializer() },
			Deserializer: func(p []byte) smithy.ShapeDeserializer { return internaljson.NewShapeDeserializer(p) },
			ContentType:  "application/json",
			ErrorInfo:    internaljson.EventStreamErrorInfo,
		},
	}
}

// Protocol implements aws.protocols#restJson1.
type Protocol struct {
	bufs        *internalsync.BufferPool
	eventstream *internales.Codec
}

var _ smithyhttp.ClientProtocol = (*Protocol)(nil)

// ID identifies the protocol.
func (*Protocol) ID() smithy.ShapeID {
	return smithy.ShapeID{Namespace: "aws.protocols", Name: "restJson1"}
}

// SerializeRequest serializes a request for restJson1.
func (p *Protocol) SerializeRequest(
	ctx context.Context,
	op *smithy.OperationSchema,
	in smithy.Serializable,
	req *smithyhttp.Request,
) error {
	serializer, err := internalhttpbinding.NewShapeSerializer(op.Schema, req, internaljson.NewShapeSerializer(useJSONName))
	if err != nil {
		return err
	}

	if op.Input != nil {
		in.Serialize(serializer)
	}

	contentType := "application/json"
	if op.IsInputEventStream() {
		contentType = "application/vnd.amazon.eventstream"
	}
	if err := serializer.Build(in, contentType); err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	return nil
}

// DeserializeResponse deserializes a response for restJson1.
func (p *Protocol) DeserializeResponse(
	ctx context.Context,
	op *smithy.OperationSchema,
	types *smithy.TypeRegistry,
	resp *smithyhttp.Response,
	out smithy.Deserializable,
) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return p.deserializeError(types, resp)
	}

	if op.Output == nil {
		return nil
	}

	// @httpPayload + @streaming = do not touch the body at all, it's the
	// caller's problem
	if so, ok := out.(smithy.StreamingOutput); ok {
		so.SetPayloadStream(resp.Body)
		deser := internalhttpbinding.NewShapeDeserializer(resp.Response, bd(nil), nil)
		if err := out.Deserialize(deser); err != nil {
			return &smithy.DeserializationError{Err: err}
		}
		return nil
	}

	if op.IsOutputEventStream() {
		if op.IsInputEventStream() {
			if err := p.eventstream.RequireBidiHTTP2(resp.Proto, resp.ProtoMajor); err != nil {
				return err
			}
		}
		deser := internalhttpbinding.NewShapeDeserializer(resp.Response, bd(nil), nil)
		if err := out.Deserialize(deser); err != nil {
			return &smithy.DeserializationError{Err: err}
		}
		return nil
	}

	// a blob @httpPayload is handed to the caller by reference, so its buffer
	// can't be reused
	var buf *bytes.Buffer
	var err error
	if internalhttpbinding.HasBlobPayload(op.Output) {
		if buf, err = internalserde.ReadPayloadBlob(resp.Body, resp.ContentLength); err != nil {
			return &smithy.DeserializationError{Err: err}
		}
	} else {
		if buf, err = p.bufs.Get(resp.Body); err != nil {
			return &smithy.DeserializationError{Err: err}
		}
		defer p.bufs.Put(buf)
	}

	payload := buf.Bytes()
	deser := internalhttpbinding.NewShapeDeserializer(resp.Response, bd(payload), payload)
	if err := out.Deserialize(deser); err != nil {
		return &smithy.DeserializationError{Err: err}
	}

	return nil
}

// HasInitialEventMessage is false for REST-style protocols with HTTP bindings.
func (*Protocol) HasInitialEventMessage() bool {
	return false
}

// SerializeEventMessage implements [smithyhttp.ClientProtocol].
func (p *Protocol) SerializeEventMessage(schema, variant *smithy.Schema, v smithy.Serializable, w io.Writer) error {
	return p.eventstream.SerializeEventMessage(schema, variant, v, w)
}

// DeserializeEventMessage implements [smithyhttp.ClientProtocol].
func (p *Protocol) DeserializeEventMessage(schema *smithy.Schema, types *smithy.TypeRegistry, r io.Reader) (smithy.Deserializable, error) {
	return p.eventstream.DeserializeEventMessage(schema, types, r)
}

// SerializeInitialRequest implements [smithyhttp.ClientProtocol].
func (p *Protocol) SerializeInitialRequest(schema *smithy.Schema, v smithy.Serializable, w io.Writer) error {
	return p.eventstream.SerializeInitialRequest(schema, v, w)
}

// DeserializeInitialResponse implements [smithyhttp.ClientProtocol].
func (p *Protocol) DeserializeInitialResponse(schema *smithy.Schema, r io.Reader, out smithy.Deserializable) error {
	return p.eventstream.DeserializeInitialResponse(schema, r, out)
}

func (p *Protocol) deserializeError(types *smithy.TypeRegistry, response *smithyhttp.Response) error {
	var errorBuffer bytes.Buffer
	if _, err := io.Copy(&errorBuffer, response.Body); err != nil {
		return &smithy.DeserializationError{Err: fmt.Errorf("failed to copy error response body, %w", err)}
	}
	errorBody := bytes.NewReader(errorBuffer.Bytes())

	errorCode := "UnknownError"
	errorMessage := errorCode

	headerCode := response.Header.Get("X-Amzn-ErrorType")

	var buff [1024]byte
	ringBuffer := smithyio.NewRingBuffer(buff[:])

	body := io.TeeReader(errorBody, ringBuffer)
	decoder := json.NewDecoder(body)
	decoder.UseNumber()
	bodyInfo, err := internaljson.GetProtocolErrorInfo(decoder)
	if err != nil {
		var snapshot bytes.Buffer
		io.Copy(&snapshot, ringBuffer)
		return &smithy.DeserializationError{
			Err:      fmt.Errorf("failed to decode response body, %w", err),
			Snapshot: snapshot.Bytes(),
		}
	}

	errorBody.Seek(0, io.SeekStart)
	if typ, ok := internaljson.ResolveProtocolErrorType(headerCode, bodyInfo); ok {
		errorCode = typ
	}
	if len(bodyInfo.Message) != 0 {
		errorMessage = bodyInfo.Message
	}

	errorCode = internaljson.SanitizeErrorCode(errorCode)

	perr, ok := types.DeserializableError(errorCode)
	if !ok {
		return &smithy.GenericAPIError{
			Code:    errorCode,
			Message: errorMessage,
		}
	}

	errorBody.Seek(0, io.SeekStart)
	errorBytes, _ := io.ReadAll(errorBody)

	deser := internalhttpbinding.NewShapeDeserializer(response.Response, bd(errorBytes), errorBytes)
	if err := perr.Deserialize(deser); err != nil {
		return &smithy.DeserializationError{Err: err}
	}

	return perr
}

func bd(payload []byte) smithy.ShapeDeserializer {
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	return internaljson.NewShapeDeserializer(payload, useJSONName)
}

func useJSONName(o *internaljson.Options) {
	o.UseJSONName = true
}
