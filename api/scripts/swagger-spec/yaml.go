package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

func parse(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected a single YAML document")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("expected a YAML mapping")
	}
	// Decode as well to detect duplicate keys and invalid or excessive aliases.
	var value any
	if err := doc.Decode(&value); err != nil {
		return nil, err
	}
	return clone(doc.Content[0]), nil
}

func clone(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.AliasNode {
		return clone(n.Alias)
	}
	result := *n
	result.Anchor = ""
	result.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		result.Content[i] = clone(child)
	}
	return &result
}

func mapping() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

func sequence(values ...string) *yaml.Node {
	result := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, value := range values {
		result.Content = append(result.Content, quotedScalar(value))
	}
	return result
}

func scalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func quotedScalar(s string) *yaml.Node {
	n := scalar(s)
	n.Style = yaml.DoubleQuotedStyle
	return n
}

func get(n *yaml.Node, keys ...string) *yaml.Node {
	for _, key := range keys {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		var value *yaml.Node
		for i := 0; i < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				value = n.Content[i+1]
				break
			}
		}
		n = value
	}
	return n
}

func text(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	return n.Value
}

func set(n *yaml.Node, key string, value *yaml.Node) {
	if value == nil {
		return
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content[i+1] = value
			return
		}
	}
	n.Content = append(n.Content, scalar(key), value)
}

func without(n *yaml.Node, keys ...string) *yaml.Node {
	result := mapping()
	for i := 0; i < len(n.Content); i += 2 {
		if !slices.Contains(keys, n.Content[i].Value) {
			result.Content = append(result.Content, n.Content[i:i+2]...)
		}
	}
	return result
}

func checkKeys(n *yaml.Node, allowed ...string) error {
	if n == nil || n.Kind != yaml.MappingNode {
		return errors.New("expected a mapping")
	}
	for i := 0; i < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if !slices.Contains(allowed, key) && !strings.HasPrefix(key, "x-") {
			return fmt.Errorf("unsupported OpenAPI field %q", key)
		}
	}
	return nil
}

func requireKeys(n *yaml.Node, keys ...string) error {
	for _, key := range keys {
		if get(n, key) == nil {
			return fmt.Errorf("missing field %q", key)
		}
	}
	return nil
}

func projectMapping(n *yaml.Node, project func(*yaml.Node) (*yaml.Node, error)) (*yaml.Node, error) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, errors.New("expected a mapping")
	}
	result := mapping()
	for i := 0; i < len(n.Content); i += 2 {
		key := n.Content[i].Value
		value, err := project(n.Content[i+1])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		set(result, key, value)
	}
	return result, nil
}

func equalNodes(a, b *yaml.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	var av, bv any
	if a.Decode(&av) != nil || b.Decode(&bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}
