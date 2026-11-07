package main

import (
	"testing"

	"gotest.tools/v3/assert"
)

func TestComparisonIgnoresOnlyLayoutDifferences(t *testing.T) {
	t.Parallel()
	original := parseTest(t, `
definitions:
  Thing:
    properties:
      first: {type: string}
      second: {type: integer}
paths:
  /test:
    get:
      parameters:
        - {in: query, name: limit, type: integer}
        - in: body
          name: body
          schema:
            properties:
              first: {type: string}
              second: {type: integer}
      responses:
        200:
          description: OK
          schema:
            properties:
              first: {type: string}
              second: {type: integer}
`)
	for _, name := range []string{"parameter placement and status key type", "definition order", "body order", "response order"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			changed := clone(original)
			op := get(changed, "paths", "/test", "get")
			params := get(op, "parameters")
			responses := get(op, "responses")
			switch name {
			case "parameter placement and status key type":
				params.Content[0], params.Content[1] = params.Content[1], params.Content[0]
				responses.Content[0] = scalar("200")
			case "definition order":
				properties := get(changed, "definitions", "Thing", "properties")
				properties.Content = append(properties.Content[2:], properties.Content[:2]...)
			case "body order":
				properties := get(params.Content[1], "schema", "properties")
				properties.Content = append(properties.Content[2:], properties.Content[:2]...)
			case "response order":
				properties := get(responses, "200", "schema", "properties")
				properties.Content = append(properties.Content[2:], properties.Content[:2]...)
			}
			assert.Equal(t, equalNodes(comparisonSpec(changed), comparisonSpec(original)), name == "parameter placement and status key type")
		})
	}
}

func TestInvalidYAML(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"", "[]", "key: one\nkey: two", "key: *missing", "key: &cycle {value: *cycle}", "key: one\n---\nkey: two"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			_, err := parse([]byte(source))
			assert.Assert(t, err != nil)
		})
	}
}

func TestRenderRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	source := readSpec(t, "openapi")
	_, err := render(source, []byte("invalid: ["))
	assert.ErrorContains(t, err, "parse previous Swagger")
	_, err = render([]byte("invalid: ["), nil)
	assert.ErrorContains(t, err, "parse OpenAPI")
}
