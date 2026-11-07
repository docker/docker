package main

import (
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

func projectSchema(schema *yaml.Node) (*yaml.Node, error) {
	if schema == nil {
		return nil, errors.New("missing schema")
	}
	if schema.Kind == yaml.ScalarNode && schema.Tag == "!!bool" {
		return schema, nil // additionalProperties
	}
	if schema.Kind != yaml.MappingNode {
		return nil, errors.New("expected a schema mapping")
	}
	// Nullability uses x-nullable, not type unions or OpenAPI 3.0 nullable.
	for _, key := range []string{"anyOf", "oneOf", "not", "const", "examples", "$defs", "discriminator", "nullable", "writeOnly", "deprecated"} {
		if get(schema, key) != nil {
			return nil, fmt.Errorf("model projection does not support %s", key)
		}
	}
	if typ := get(schema, "type"); typ != nil && typ.Kind == yaml.SequenceNode {
		return nil, errors.New("model projection does not support type unions")
	}
	result := without(schema)
	if ref := get(schema, "$ref"); ref != nil {
		const prefix = "#/components/schemas/"
		if !strings.HasPrefix(ref.Value, prefix) {
			return nil, fmt.Errorf("model projection requires a local schema reference: %q", ref.Value)
		}
		// go-swagger honors description and generator extensions beside $ref.
		set(result, "$ref", quotedScalar("#/definitions/"+strings.TrimPrefix(ref.Value, prefix)))
	}
	if properties := get(schema, "properties"); properties != nil {
		projected, err := projectMapping(properties, projectSchema)
		if err != nil {
			return nil, fmt.Errorf("properties: %w", err)
		}
		set(result, "properties", projected)
	}
	for _, key := range []string{"items", "additionalProperties"} {
		if child := get(schema, key); child != nil {
			projected, err := projectSchema(child)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			set(result, key, projected)
		}
	}
	if allOf := get(schema, "allOf"); allOf != nil {
		if allOf.Kind != yaml.SequenceNode {
			return nil, errors.New("allOf must be a sequence")
		}
		projected := sequence()
		for _, child := range allOf.Content {
			value, err := projectSchema(child)
			if err != nil {
				return nil, fmt.Errorf("allOf: %w", err)
			}
			projected.Content = append(projected.Content, value)
		}
		set(result, "allOf", projected)
	}
	return result, nil
}
