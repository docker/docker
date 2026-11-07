package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// project converts the Engine's supported OpenAPI subset, not arbitrary OpenAPI.
func project(spec *yaml.Node) (*yaml.Node, error) {
	if err := checkKeys(spec, "openapi", "jsonSchemaDialect", "servers", "info", "tags", "components", "paths"); err != nil {
		return nil, err
	}
	if text(get(spec, "openapi")) != "3.2.0" {
		return nil, errors.New("Swagger projection requires OpenAPI 3.2.0")
	}
	if text(get(spec, "jsonSchemaDialect")) != "https://spec.openapis.org/oas/3.1/dialect/base" {
		return nil, errors.New("unsupported schema dialect")
	}
	if err := requireKeys(spec, "info", "tags"); err != nil {
		return nil, err
	}
	servers := get(spec, "servers")
	if servers == nil || servers.Kind != yaml.SequenceNode || len(servers.Content) != 1 || servers.Content[0].Kind != yaml.MappingNode || len(servers.Content[0].Content) != 2 {
		return nil, errors.New("Swagger projection requires one relative server URL")
	}
	basePath := text(get(servers.Content[0], "url"))
	if !strings.HasPrefix(basePath, "/") || strings.HasPrefix(basePath, "//") || strings.Contains(basePath, "{") {
		return nil, errors.New("Swagger projection requires a relative server URL without variables")
	}
	components := get(spec, "components")
	if err := checkKeys(components, "schemas"); err != nil {
		return nil, fmt.Errorf("components: %w", err)
	}
	definitions, err := projectMapping(get(components, "schemas"), projectSchema)
	if err != nil {
		return nil, fmt.Errorf("schemas: %w", err)
	}
	result := mapping()
	set(result, "swagger", scalar("2.0"))
	// These legacy Swagger defaults are compatibility policy, not OpenAPI fields.
	set(result, "schemes", sequence("http", "https"))
	set(result, "produces", sequence("application/json", "text/plain"))
	set(result, "consumes", sequence("application/json", "text/plain"))
	set(result, "basePath", quotedScalar(basePath))
	set(result, "info", get(spec, "info"))
	set(result, "tags", get(spec, "tags"))
	set(result, "definitions", definitions)
	paths, err := projectMapping(get(spec, "paths"), func(item *yaml.Node) (*yaml.Node, error) {
		if err := checkKeys(item, "get", "put", "post", "delete", "options", "head", "patch"); err != nil {
			return nil, err
		}
		methods := mapping()
		for i := 0; i < len(item.Content); i += 2 {
			method, value := item.Content[i].Value, item.Content[i+1]
			if !strings.HasPrefix(method, "x-") {
				var err error
				value, err = projectOperation(value, result)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", method, err)
				}
			}
			set(methods, method, value)
		}
		return methods, nil
	})
	if err != nil {
		return nil, fmt.Errorf("paths: %w", err)
	}
	set(result, "paths", paths)
	return result, nil
}

func projectParameter(value *yaml.Node) (*yaml.Node, error) {
	if err := checkKeys(value, "name", "in", "description", "required", "schema", "style", "explode"); err != nil {
		return nil, err
	}
	if err := requireKeys(value, "name", "in"); err != nil {
		return nil, err
	}
	schema, err := projectSchema(get(value, "schema"))
	if err != nil {
		return nil, err
	}
	typ := text(get(schema, "type"))
	if !slices.Contains([]string{"string", "integer", "number", "boolean", "array"}, typ) {
		return nil, errors.New("Swagger non-body parameters require a primitive or array type")
	}
	result := without(value, "schema", "style", "explode")
	for i := 0; i < len(schema.Content); i += 2 {
		set(result, schema.Content[i].Value, schema.Content[i+1])
	}
	style, explode := get(value, "style"), get(value, "explode")
	if typ == "array" {
		if text(get(value, "in")) != "query" || (style != nil && style.Value != "form") {
			return nil, errors.New("only form-style query arrays are supported")
		}
		format := "multi"
		if explode != nil && explode.Tag == "!!bool" && explode.Value == "false" {
			format = "csv"
		}
		set(result, "collectionFormat", quotedScalar(format))
	} else if style != nil || explode != nil {
		return nil, errors.New("serialization options are only supported on query arrays")
	}
	return result, nil
}

func contentSchema(content *yaml.Node, requestBody bool) (*yaml.Node, error) {
	if content == nil || content.Kind != yaml.MappingNode || len(content.Content) == 0 {
		return nil, errors.New("body content must be a nonempty mapping")
	}
	var schema *yaml.Node
	for i := 0; i < len(content.Content); i += 2 {
		media := content.Content[i+1]
		if err := checkKeys(media, "schema", "example"); err != nil {
			return nil, err
		}
		next := get(media, "schema")
		// Swagger requests have only a schema example; responses have per-media examples.
		if requestBody && next != nil && next.Kind == yaml.MappingNode {
			next = without(next)
			set(next, "example", get(media, "example"))
		}
		if i == 0 {
			schema = next
		} else if !equalNodes(schema, next) {
			if requestBody {
				return nil, errors.New("Swagger requires the same schema and effective example for all request media types")
			}
			return nil, errors.New("Swagger requires the same schema for all media types of a body")
		}
	}
	if schema == nil {
		return nil, nil
	}
	return projectSchema(schema)
}

func projectResponse(value *yaml.Node) (*yaml.Node, error) {
	if err := checkKeys(value, "description", "headers", "content"); err != nil {
		return nil, err
	}
	if err := requireKeys(value, "description"); err != nil {
		return nil, err
	}
	result := without(value, "headers", "content")
	if headers := get(value, "headers"); headers != nil {
		projected, err := projectMapping(headers, func(header *yaml.Node) (*yaml.Node, error) {
			if err := checkKeys(header, "description", "schema"); err != nil {
				return nil, err
			}
			schema, err := projectSchema(get(header, "schema"))
			if err != nil {
				return nil, err
			}
			projectedHeader := without(header, "schema")
			for i := 0; i < len(schema.Content); i += 2 {
				set(projectedHeader, schema.Content[i].Value, schema.Content[i+1])
			}
			return projectedHeader, nil
		})
		if err != nil {
			return nil, fmt.Errorf("headers: %w", err)
		}
		set(result, "headers", projected)
	}
	if content := get(value, "content"); content != nil {
		schema, err := contentSchema(content, false)
		if err != nil {
			return nil, err
		}
		set(result, "schema", schema)
		examples := mapping()
		for i := 0; i < len(content.Content); i += 2 {
			set(examples, content.Content[i].Value, get(content.Content[i+1], "example"))
		}
		if len(examples.Content) != 0 {
			set(result, "examples", examples)
		}
	}
	return result, nil
}

func projectOperation(value, defaults *yaml.Node) (*yaml.Node, error) {
	if err := checkKeys(value, "summary", "description", "operationId", "tags", "parameters", "requestBody", "responses", "deprecated"); err != nil {
		return nil, err
	}
	result := without(value, "requestBody", "responses", "parameters", "x-consumes")
	parameters := sequence()
	if params := get(value, "parameters"); params != nil {
		if params.Kind != yaml.SequenceNode {
			return nil, errors.New("parameters must be a sequence")
		}
		for _, param := range params.Content {
			projected, err := projectParameter(param)
			if err != nil {
				return nil, fmt.Errorf("parameter %q: %w", text(get(param, "name")), err)
			}
			parameters.Content = append(parameters.Content, projected)
		}
		set(result, "parameters", parameters)
	}
	consumes := get(value, "x-consumes")
	if consumes == nil {
		consumes = get(defaults, "consumes")
	}
	if body := get(value, "requestBody"); body != nil {
		if err := checkKeys(body, "description", "required", "content"); err != nil {
			return nil, err
		}
		if err := requireKeys(body, "x-codegen-request-body-name"); err != nil {
			return nil, err
		}
		if get(value, "x-consumes") != nil {
			return nil, errors.New("requestBody and x-consumes cannot both specify media types")
		}
		content := get(body, "content")
		schema, err := contentSchema(content, true)
		if err != nil {
			return nil, err
		}
		if schema == nil {
			return nil, errors.New("request bodies require a schema")
		}
		consumes = sequence()
		for i := 0; i < len(content.Content); i += 2 {
			consumes.Content = append(consumes.Content, quotedScalar(content.Content[i].Value))
		}
		param := mapping()
		set(param, "name", get(body, "x-codegen-request-body-name"))
		set(param, "in", quotedScalar("body"))
		set(param, "description", get(body, "description"))
		set(param, "required", get(body, "required"))
		set(param, "schema", schema)
		parameters.Content = append(parameters.Content, param)
		set(result, "parameters", parameters)
	}
	if !equalNodes(consumes, get(defaults, "consumes")) {
		set(result, "consumes", consumes)
	}
	responses := get(value, "responses")
	projected, err := projectMapping(responses, projectResponse)
	if err != nil {
		return nil, fmt.Errorf("responses: %w", err)
	}
	// Swagger has operation-wide media types, not per-response media types.
	produces := sequence()
	seen := make(map[string]bool)
	for i := 1; i < len(responses.Content); i += 2 {
		content := get(responses.Content[i], "content")
		if content == nil {
			continue
		}
		for j := 0; j < len(content.Content); j += 2 {
			media := content.Content[j].Value
			if !seen[media] {
				produces.Content = append(produces.Content, quotedScalar(media))
				seen[media] = true
			}
		}
	}
	if !equalNodes(produces, get(defaults, "produces")) {
		set(result, "produces", produces)
	}
	set(result, "responses", projected)
	return result, nil
}
