package main

import (
	"bytes"
	"os"
	"testing"

	"go.yaml.in/yaml/v3"
	"gotest.tools/v3/assert"
)

func parseTest(t *testing.T, source string) *yaml.Node {
	t.Helper()
	n, err := parse([]byte(source))
	assert.NilError(t, err)
	return n
}

func readSpec(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../" + name + ".yaml")
	assert.NilError(t, err)
	return data
}

func decoded(t *testing.T, n *yaml.Node) any {
	t.Helper()
	var value any
	assert.NilError(t, n.Decode(&value))
	return value
}

func assertYAML(t *testing.T, actual *yaml.Node, expected string) {
	t.Helper()
	assert.DeepEqual(t, decoded(t, actual), decoded(t, parseTest(t, expected)))
}

func mappingKeys(n *yaml.Node) []string {
	var keys []string
	if n != nil {
		for i := 0; i < len(n.Content); i += 2 {
			keys = append(keys, n.Content[i].Value)
		}
	}
	return keys
}

func TestCheckedInSwagger(t *testing.T) {
	t.Parallel()
	source, previous := readSpec(t, "openapi"), readSpec(t, "swagger")
	spec := parseTest(t, string(source))
	original := clone(spec)
	projected, err := project(spec)
	assert.NilError(t, err)
	assert.DeepEqual(t, decoded(t, comparisonSpec(projected)), decoded(t, comparisonSpec(parseTest(t, string(previous)))))
	assert.DeepEqual(t, spec, original)
	assert.DeepEqual(t, mappingKeys(get(projected, "paths")), mappingKeys(get(spec, "paths")))
	for _, path := range mappingKeys(get(spec, "paths")) {
		assert.DeepEqual(t, mappingKeys(get(projected, "paths", path)), mappingKeys(get(spec, "paths", path)))
	}
	schemas := get(spec, "components", "schemas")
	assert.DeepEqual(t, mappingKeys(get(projected, "definitions")), mappingKeys(schemas))
	for _, name := range mappingKeys(schemas) {
		assert.DeepEqual(t, mappingKeys(get(projected, "definitions", name, "properties")), mappingKeys(get(schemas, name, "properties")))
	}

	unchanged, err := render(source, previous)
	assert.NilError(t, err)
	assert.Equal(t, string(unchanged), string(previous))
	fresh, err := render(source, nil)
	assert.NilError(t, err)
	header, _, found := bytes.Cut(previous, []byte("\nswagger:"))
	assert.Assert(t, found)
	assert.Assert(t, bytes.HasPrefix(fresh, header))
	assert.DeepEqual(t, decoded(t, comparisonSpec(parseTest(t, string(fresh)))), decoded(t, comparisonSpec(projected)))
}

func TestMultilineJSONExamples(t *testing.T) {
	t.Parallel()
	source := []byte(`openapi: 3.2.0
jsonSchemaDialect: https://spec.openapis.org/oas/3.1/dialect/base
servers:
  - url: /v1.56
info:
  title: Engine API
  version: '1.56'
tags: []
components:
  schemas:
    Storage:
      type: object
      example:
        {
          "MergedDir": "/old/merged",
          "é": {"brackets": "[}]", "quote": "\""},
          "entries": [
            {"value": 1},
            {"value": 2}
            ]
        }
    Inline:
      type: object
      example: {
        "value": "/old/merged"
        }
paths: {}
`)
	const example = `    example: {
        "MergedDir": "/old/merged",
        "é": {"brackets": "[}]", "quote": "\""},
        "entries": [
          {"value": 1},
          {"value": 2}
          ]
      }
`
	const inlineExample = `    example: {
      "value": "/old/merged"
      }
`
	previous, err := render(source, nil)
	assert.NilError(t, err)
	assert.Assert(t, bytes.Contains(previous, []byte(example)))
	assert.Assert(t, bytes.Contains(previous, []byte(inlineExample)))
	// Force regeneration, not the unchanged-document fast path.
	source = bytes.ReplaceAll(source, []byte("/old/merged"), []byte("/new/merged"))
	result, err := render(source, previous)
	assert.NilError(t, err)
	assert.Assert(t, bytes.Contains(result, bytes.ReplaceAll([]byte(example), []byte("/old/merged"), []byte("/new/merged"))))
	assert.Assert(t, bytes.Contains(result, bytes.ReplaceAll([]byte(inlineExample), []byte("/old/merged"), []byte("/new/merged"))))
	projected, err := project(parseTest(t, string(source)))
	assert.NilError(t, err)
	assert.DeepEqual(t, decoded(t, parseTest(t, string(result))), decoded(t, projected))
}

func TestOpaqueSchemaData(t *testing.T) {
	t.Parallel()
	schema := parseTest(t, `
type: object
example: &opaque {$ref: '#/components/schemas/NotAReference', nullable: true}
default: *opaque
x-custom: *opaque
additionalProperties: false
properties:
  nullable: {type: boolean, x-nullable: false}
  nested:
    type: array
    items:
      allOf:
        - {$ref: '#/components/schemas/Thing'}
`)
	result, err := projectSchema(schema)
	assert.NilError(t, err)
	for _, key := range []string{"example", "default", "x-custom", "additionalProperties"} {
		assert.DeepEqual(t, decoded(t, get(result, key)), decoded(t, get(schema, key)))
	}
	assertYAML(t, get(result, "properties", "nullable"), `{type: boolean, x-nullable: false}`)
	assertYAML(t, get(result, "properties", "nested"), `{type: array, items: {allOf: [{$ref: '#/definitions/Thing'}]}}`)
}

func TestQueryArraySerialization(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, options, expected string }{
		{"default", "", "multi"},
		{"repeated", ", style: form, explode: true", "multi"},
		{"comma separated", ", style: form, explode: false", "csv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result, err := projectParameter(parseTest(t, `{name: names, in: query, schema: {type: array, items: {type: string}}`+tc.options+`}`))
			assert.NilError(t, err)
			assertYAML(t, result, `{name: names, in: query, type: array, items: {type: string}, collectionFormat: `+tc.expected+`}`)
		})
	}
}

func TestRequestAndResponseMediaTypes(t *testing.T) {
	t.Parallel()
	value := parseTest(t, `
requestBody:
  x-codegen-request-body-name: archive
  required: true
  description: Archive to import
  content:
    application/x-tar:
      schema: {type: string, format: binary}
responses:
  200:
    description: stream
    content:
      application/octet-stream: {}
  400:
    description: bad request
    content:
      application/json:
        schema: {$ref: '#/components/schemas/ErrorResponse'}
        example: {message: bad archive}
      text/plain:
        schema: {$ref: '#/components/schemas/ErrorResponse'}
        example: bad archive
`)
	result, err := projectOperation(value, parseTest(t, `{consumes: [application/json], produces: [application/json]}`))
	assert.NilError(t, err)
	for field, expected := range map[string]string{
		"consumes": "- \"application/x-tar\"\n",
		"produces": "- \"application/octet-stream\"\n- \"application/json\"\n- \"text/plain\"\n",
	} {
		encoded, err := yaml.Marshal(get(result, field))
		assert.NilError(t, err)
		assert.Equal(t, string(encoded), expected)
	}
	assertYAML(t, result, `
consumes: [application/x-tar]
produces: [application/octet-stream, application/json, text/plain]
parameters:
  - name: archive
    in: body
    required: true
    description: Archive to import
    schema: {type: string, format: binary}
responses:
  "200": {description: stream}
  "400":
    description: bad request
    schema: {$ref: '#/definitions/ErrorResponse'}
    examples:
      application/json: {message: bad archive}
      text/plain: bad archive
`)
}

func TestRequestBodyExamples(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, content, schema string }{
		{"media example", `{application/json: {schema: {type: object}, example: {name: demo}}}`, `{type: object, example: {name: demo}}`},
		{"override schema example", `{application/json: {schema: {type: string, example: old}, example: new}}`, `{type: string, example: new}`},
		{"matching effective examples", `{application/json: {schema: {type: string, example: old}, example: new}, text/plain: {schema: {type: string, example: new}}}`, `{type: string, example: new}`},
		{"false override", `{application/json: {schema: {type: boolean, example: true}, example: false}}`, `{type: boolean, example: false}`},
		{"null override", `{application/json: {schema: {type: object, example: {}}, example: null}}`, `{type: object, example: null}`},
		{"different media examples", `{application/json: {schema: {type: string}, example: first}, text/plain: {schema: {type: string}, example: second}}`, ""},
		{"conflicting fallback", `{application/json: {schema: {type: string, example: old}, example: new}, text/plain: {schema: {type: string, example: old}}}`, ""},
		{"missing example", `{application/json: {schema: {type: string}, example: demo}, text/plain: {schema: {type: string}}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			value := parseTest(t, `{requestBody: {x-codegen-request-body-name: body, content: `+tc.content+`}, responses: {}}`)
			original := clone(value)
			result, err := projectOperation(value, parseTest(t, `{consumes: [], produces: []}`))
			assert.DeepEqual(t, value, original)
			if tc.schema == "" {
				assert.ErrorContains(t, err, "same schema and effective example")
				return
			}
			assert.NilError(t, err)
			parameters := get(result, "parameters")
			assert.Equal(t, len(parameters.Content), 1)
			assertYAML(t, parameters.Content[0], `{name: body, in: body, schema: `+tc.schema+`}`)
		})
	}
}

func TestUnsupportedSchemas(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"anyOf", "oneOf", "not", "const", "examples", "$defs", "discriminator", "nullable", "writeOnly", "deprecated"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			_, err := projectSchema(parseTest(t, `{`+key+`: true}`))
			assert.ErrorContains(t, err, "model projection does not support "+key)
		})
	}
	_, err := projectSchema(parseTest(t, `{type: [object, 'null']}`))
	assert.ErrorContains(t, err, "type unions")
	_, err = projectSchema(parseTest(t, `{$ref: 'other.yaml#/Thing'}`))
	assert.ErrorContains(t, err, "local schema reference")
}

func TestUnrepresentableContent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, content, error string }{
		{"different schemas", `{application/json: {schema: {type: object}}, text/plain: {schema: {type: string}}}`, "same schema"},
		{"missing schema", `{application/json: {schema: {type: object}}, text/plain: {}}`, "same schema"},
		{"streaming schema", `{application/json: {itemSchema: {type: object}}}`, "unsupported OpenAPI field"},
		{"empty content", `{}`, "nonempty mapping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := projectResponse(parseTest(t, `{description: response, content: `+tc.content+`}`))
			assert.ErrorContains(t, err, tc.error)
		})
	}
}

func TestUnsupportedOperations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, operation, error string }{
		{"callbacks", `{callbacks: {}, responses: {}}`, "unsupported OpenAPI field"},
		{"conflicting media", `{x-consumes: [], requestBody: {x-codegen-request-body-name: body, content: {}}, responses: {}}`, "cannot both specify"},
		{"missing body schema", `{requestBody: {x-codegen-request-body-name: body, content: {text/plain: {}}}, responses: {}}`, "request bodies require a schema"},
		{"missing responses", `{}`, "expected a mapping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := projectOperation(parseTest(t, tc.operation), parseTest(t, `{consumes: [], produces: []}`))
			assert.ErrorContains(t, err, tc.error)
		})
	}
}

func TestUnsupportedParameters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, parameter, error string }{
		{"object", `{name: n, in: query, schema: {type: object}}`, "primitive or array"},
		{"header array", `{name: n, in: header, schema: {type: array}}`, "form-style query arrays"},
		{"array style", `{name: n, in: query, style: spaceDelimited, schema: {type: array}}`, "form-style query arrays"},
		{"scalar style", `{name: n, in: query, style: form, schema: {type: string}}`, "only supported on query arrays"},
		{"scalar explode", `{name: n, in: query, explode: true, schema: {type: string}}`, "only supported on query arrays"},
		{"missing schema", `{name: n, in: query}`, "missing schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := projectParameter(parseTest(t, tc.parameter))
			assert.ErrorContains(t, err, tc.error)
		})
	}
}

func TestUnsupportedSpecification(t *testing.T) {
	t.Parallel()
	original := parseTest(t, `
openapi: 3.2.0
jsonSchemaDialect: https://spec.openapis.org/oas/3.1/dialect/base
servers: [{url: /v1}]
info: {}
tags: []
components: {schemas: {}}
paths: {}
`)
	for _, tc := range []struct{ name, field, value, error string }{
		{"version", "openapi", "3.0.3", "requires OpenAPI 3.2.0"},
		{"dialect", "jsonSchemaDialect", "unknown", "unsupported schema dialect"},
		{"absolute server", "servers", "[{url: 'https://example.com'}]", "relative server URL"},
		{"network server", "servers", "[{url: '//example.com'}]", "relative server URL"},
		{"variable server", "servers", "[{url: '/{version}'}]", "without variables"},
		{"multiple servers", "servers", "[{url: /v1}, {url: /v2}]", "one relative server"},
		{"components", "components", "{schemas: {}, parameters: {}}", "unsupported OpenAPI field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := clone(original)
			set(spec, tc.field, get(parseTest(t, "value: "+tc.value), "value"))
			_, err := project(spec)
			assert.ErrorContains(t, err, tc.error)
		})
	}
}
