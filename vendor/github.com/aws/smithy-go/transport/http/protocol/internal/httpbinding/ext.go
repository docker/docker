package httpbinding

import (
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/traits"
)

// bindingExt caches per-schema facts about a shape's HTTP bindings that would
// otherwise be recomputed by walking its members on every request.
type bindingExt struct {
	hasBlobPayload bool
}

func getExt(s *smithy.Schema) *bindingExt {
	return smithy.SchemaExtension(s, smithy.ExtHTTPBinding, buildExt)
}

func buildExt(s *smithy.Schema) *bindingExt {
	return &bindingExt{
		hasBlobPayload: hasBlobPayload(s),
	}
}

func hasBlobPayload(output *smithy.Schema) bool {
	for _, member := range output.Members() {
		if _, ok := smithy.SchemaTrait[*traits.HTTPPayload](member); !ok {
			continue
		}
		if member.Type() == smithy.ShapeTypeBlob {
			return true
		}
	}
	return false
}
