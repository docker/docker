package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"go.yaml.in/yaml/v3"
)

// preserveJSONLayout restores multiline JSON objects and arrays from the source.
// yaml.Node retains flow style, but the YAML encoder discards its line breaks.
func preserveJSONLayout(source, output []byte, projected *yaml.Node) ([]byte, error) {
	var emitted yaml.Node
	if err := yaml.Unmarshal(output, &emitted); err != nil {
		return nil, err
	}
	var result bytes.Buffer
	position := 0
	var walk func(*yaml.Node, *yaml.Node) error
	walk = func(original, rendered *yaml.Node) error {
		if original.Kind != rendered.Kind || len(original.Content) != len(rendered.Content) {
			return errors.New("YAML encoder changed the projected structure")
		}
		src, sourceOK := jsonFragment(source, original)
		dst, outputOK := jsonFragment(output, rendered)
		if sourceOK && outputOK && bytes.Contains(src.text, []byte("\n")) {
			// Source locations survive projection, so check the value as well:
			// formatting must never restore data that projection changed.
			var value yaml.Node
			if err := yaml.Unmarshal(src.text, &value); err == nil && sameContent(value.Content[0], original) {
				result.Write(output[position:dst.start])
				lines := bytes.Split(src.text, []byte("\n"))
				result.Write(lines[0])
				sourceIndent := bytes.Repeat([]byte(" "), src.indent)
				outputIndent := strings.Repeat(" ", dst.indent)
				if original.Column == src.indent+1 {
					// The encoder moved the opening delimiter beside the key.
					// Keep continuation lines indented beyond that key.
					outputIndent += "  "
				}
				for _, line := range lines[1:] {
					result.WriteByte('\n')
					if len(bytes.TrimSpace(line)) != 0 {
						result.WriteString(outputIndent)
						result.Write(bytes.TrimPrefix(line, sourceIndent))
					}
				}
				position = dst.start + len(dst.text)
				return nil
			}
		}
		for i, child := range original.Content {
			if err := walk(child, rendered.Content[i]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(projected, emitted.Content[0]); err != nil {
		return nil, err
	}
	result.Write(output[position:])
	return result.Bytes(), nil
}

// sameContent includes mapping order, which matters to generated Go models.
func sameContent(a, b *yaml.Node) bool {
	if a.Kind != b.Kind || a.Tag != b.Tag || a.Value != b.Value || len(a.Content) != len(b.Content) {
		return false
	}
	for i, child := range a.Content {
		if !sameContent(child, b.Content[i]) {
			return false
		}
	}
	return true
}

type jsonSource struct {
	text   []byte
	start  int
	indent int
}

func jsonFragment(data []byte, node *yaml.Node) (jsonSource, bool) {
	if node.Style&yaml.FlowStyle == 0 || node.Line < 1 || node.Column < 1 {
		return jsonSource{}, false
	}
	lineStart := 0
	for line := 1; line < node.Line; line++ {
		end := bytes.IndexByte(data[lineStart:], '\n')
		if end < 0 {
			return jsonSource{}, false
		}
		lineStart += end + 1
	}
	line, _, _ := bytes.Cut(data[lineStart:], []byte("\n"))
	// YAML columns count characters, not bytes.
	runes := []rune(string(line))
	if node.Column > len(runes) {
		return jsonSource{}, false
	}
	start := lineStart + len(string(runes[:node.Column-1]))
	if data[start] != '{' && data[start] != '[' {
		return jsonSource{}, false
	}
	var raw json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data[start:]))
	if err := decoder.Decode(&raw); err != nil {
		return jsonSource{}, false
	}
	return jsonSource{
		text:   data[start : start+int(decoder.InputOffset())],
		start:  start,
		indent: len(line) - len(bytes.TrimLeft(line, " ")),
	}, true
}
