// swagger-spec projects the Engine OpenAPI specification to Swagger 2.0.
package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const swaggerHeader = `# A Swagger 2.0 (a.k.a. OpenAPI) definition of the Engine API.
#
# This is used for generating API documentation and the types used by the
# client/server. See api/README.md for more information.
#
# Some style notes:
# - This file is used by ReDoc, which allows GitHub Flavored Markdown in
#   descriptions.
# - There is no maximum line length, for ease of editing and pretty diffs.
# - operationIds are in the format "NounVerb", with a singular noun.

# Code generated from openapi.yaml; DO NOT EDIT.

`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: swagger-spec openapi.yaml [swagger.yaml]")
	}
	source, err := os.ReadFile(args[0]) // #nosec G703 -- intentionally using user-provided path.
	if err != nil {
		return err
	}
	var previous []byte
	if len(args) == 2 {
		previous, err = os.ReadFile(args[1]) // #nosec G703 -- intentionally using user-provided path.
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	result, err := render(source, previous)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(result)
	return err
}

func render(source, previous []byte) ([]byte, error) {
	spec, err := parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse OpenAPI: %w", err)
	}
	projected, err := project(spec)
	if err != nil {
		return nil, err
	}
	if len(previous) != 0 {
		old, err := parse(previous)
		if err != nil {
			return nil, fmt.Errorf("parse previous Swagger: %w", err)
		}
		if equalNodes(comparisonSpec(old), comparisonSpec(projected)) {
			return previous, nil
		}
	}
	var buf bytes.Buffer
	buf.WriteString(swaggerHeader)
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(projected); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return preserveJSONLayout(source, buf.Bytes(), projected)
}

// comparisonSpec ignores parameter placement and YAML status-key types, but not
// schema property order, which affects go-swagger's generated field order.
func comparisonSpec(spec *yaml.Node) *yaml.Node {
	spec = clone(spec)
	if definitions := get(spec, "definitions"); definitions != nil {
		for i := 1; i < len(definitions.Content); i += 2 {
			compareSchemaOrder(definitions.Content[i])
		}
	}
	paths := get(spec, "paths")
	if paths == nil {
		return spec
	}
	for i := 1; i < len(paths.Content); i += 2 {
		methods := paths.Content[i]
		for j := 0; j < len(methods.Content); j += 2 {
			if strings.HasPrefix(methods.Content[j].Value, "x-") {
				continue
			}
			op := methods.Content[j+1]
			if responses := get(op, "responses"); responses != nil {
				for k := 0; k < len(responses.Content); k += 2 {
					responses.Content[k] = scalar(responses.Content[k].Value)
					compareSchemaOrder(get(responses.Content[k+1], "schema"))
				}
			}
			parameters := get(op, "parameters")
			if parameters == nil {
				parameters = sequence()
				set(op, "parameters", parameters)
			}
			for _, param := range parameters.Content {
				compareSchemaOrder(get(param, "schema"))
			}
			slices.SortStableFunc(parameters.Content, func(a, b *yaml.Node) int {
				if c := cmp.Compare(text(get(a, "in")), text(get(b, "in"))); c != 0 {
					return c
				}
				return cmp.Compare(text(get(a, "name")), text(get(b, "name")))
			})
		}
	}
	return spec
}

func compareSchemaOrder(schema *yaml.Node) {
	if schema == nil || schema.Kind != yaml.MappingNode {
		return
	}
	if properties := get(schema, "properties"); properties != nil {
		ordered := sequence()
		for i := 0; i < len(properties.Content); i += 2 {
			compareSchemaOrder(properties.Content[i+1])
			pair := sequence()
			pair.Content = properties.Content[i : i+2]
			ordered.Content = append(ordered.Content, pair)
		}
		set(schema, "properties", ordered)
	}
	for _, key := range []string{"items", "additionalProperties"} {
		compareSchemaOrder(get(schema, key))
	}
	if allOf := get(schema, "allOf"); allOf != nil {
		for _, child := range allOf.Content {
			compareSchemaOrder(child)
		}
	}
}
