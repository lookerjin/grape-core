package plugin

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Definition is the supported Markdown front matter and literal instruction body.
type Definition struct {
	Plugin       string   `yaml:"-"`
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Model        string   `yaml:"model"`
	Tools        []string `yaml:"tools"`
	Arguments    []string `yaml:"arguments"`
	ArgumentHint string   `yaml:"argument-hint"`
	Instruction  string   `yaml:"-"`
	Kind         Kind     `yaml:"-"`
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)
var placeholder = regexp.MustCompile(`\{\{([^{}]*)\}\}`)

func ParseDefinition(c Component) (Definition, error) {
	d := Definition{Kind: c.Kind, Plugin: c.Plugin}
	f, err := os.Open(c.Path)
	if err != nil {
		return d, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return d, err
	}
	if len(raw) > 65536 {
		return d, fmt.Errorf("definition exceeds 64 KiB")
	}
	if !utf8.Valid(raw) {
		return d, fmt.Errorf("definition must be UTF-8")
	}
	body := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if strings.HasPrefix(body, "---\n") {
		header, rest, ok := strings.Cut(body[4:], "\n---\n")
		if !ok {
			return d, fmt.Errorf("unterminated front matter")
		}
		dec := yaml.NewDecoder(bytes.NewBufferString(header))
		dec.KnownFields(true)
		if err := dec.Decode(&d); err != nil {
			return d, err
		}
		body = rest
	}
	if d.Name == "" {
		d.Name = c.Name
	}
	if d.Name != c.Name || !identifier.MatchString(d.Name) {
		return d, fmt.Errorf("invalid name for %q", c.Name)
	}
	d.Instruction = strings.TrimSpace(body)
	if d.Instruction == "" {
		return d, fmt.Errorf("empty instruction")
	}
	if c.Kind == KindAgent {
		if d.ArgumentHint != "" || len(d.Arguments) > 0 || (d.Model != "" && d.Model != "inherit") {
			return d, fmt.Errorf("agent supports inherited model and named tool grants only")
		}
	} else if c.Kind == KindCommand {
		if d.Model != "" || len(d.Tools) > 0 {
			return d, fmt.Errorf("command cannot declare model or tools")
		}
		// Normalize the Claude-style whole argument string into the same
		// single-pass template used by named arguments.
		if strings.Contains(body, "$ARGUMENTS") {
			if len(d.Arguments) > 0 || strings.Contains(body, "$ARGUMENTS[") {
				return d, fmt.Errorf("whole and named/positional arguments cannot be combined")
			}
			d.Arguments = []string{"ARGUMENTS"}
			body = strings.ReplaceAll(body, "$ARGUMENTS", "{{ARGUMENTS}}")
			d.Instruction = strings.TrimSpace(body)
		}
		remainder := placeholder.ReplaceAllString(body, "")
		if strings.Contains(remainder, "{{") || strings.Contains(remainder, "}}") {
			return d, fmt.Errorf("malformed command placeholder")
		}
		for _, m := range placeholder.FindAllStringSubmatch(body, -1) {
			if !slices.Contains(d.Arguments, m[1]) {
				return d, fmt.Errorf("undeclared argument %q", m[1])
			}
		}
	} else {
		return d, fmt.Errorf("unsupported component %q", c.Kind)
	}
	for _, names := range [][]string{d.Arguments, d.Tools} {
		for i, n := range names {
			if !identifier.MatchString(n) || slices.Contains(names[:i], n) {
				return d, fmt.Errorf("invalid or duplicate name %q", n)
			}
		}
	}
	for _, n := range d.Arguments {
		if !strings.Contains(body, "{{"+n+"}}") {
			return d, fmt.Errorf("unused argument %q", n)
		}
	}
	return d, nil
}

// Render substitutes each declared parameter once per occurrence, without reinterpreting its value.
func (d Definition) Render(args map[string]string) (string, error) {
	if d.Kind != KindCommand || len(args) != len(d.Arguments) {
		return "", fmt.Errorf("incorrect command arguments")
	}
	for _, n := range d.Arguments {
		if !utf8.ValidString(args[n]) {
			return "", fmt.Errorf("invalid UTF-8 in argument %q", n)
		}
		if strings.TrimSpace(args[n]) == "" {
			return "", fmt.Errorf("missing argument %q", n)
		}
	}
	const maxRendered = 1 << 20
	size := int64(len(d.Instruction))
	for _, m := range placeholder.FindAllStringSubmatch(d.Instruction, -1) {
		size += int64(len(args[m[1]])) - int64(len(m[0]))
		if size > maxRendered {
			return "", fmt.Errorf("command exceeds 1 MiB")
		}
	}
	return placeholder.ReplaceAllStringFunc(d.Instruction, func(s string) string { return args[s[2:len(s)-2]] }), nil
}
