package rules

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Rule is one instruction file.
type Rule struct {
	Source string
	Name   string
	Path   string
	Body   string
	Globs  []string
}

// Load reads markdown rules. roots are scanned in the order given, and files
// inside each root are sorted by relative path.
func Load(roots ...Source) ([]Rule, error) {
	var out []Rule
	for _, root := range roots {
		if strings.TrimSpace(root.Path) == "" {
			continue
		}
		info, err := os.Stat(root.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if !info.IsDir() {
			rule, err := readFile(root.Name, root.Path)
			if err != nil {
				return nil, err
			}
			out = append(out, rule)
			continue
		}
		var paths []string
		err = filepath.WalkDir(root.Path, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(paths)
		for _, path := range paths {
			rule, err := readFile(root.Name, path)
			if err != nil {
				return nil, err
			}
			out = append(out, rule)
		}
	}
	return out, nil
}

// Source is one rule root and the label used in summaries.
type Source struct {
	Name string
	Path string
}

// Applicable returns rules whose globs match rel, plus rules with no globs.
func Applicable(rules []Rule, rel string) []Rule {
	var out []Rule
	for _, rule := range rules {
		if len(rule.Globs) == 0 || matchAny(rule.Globs, rel) {
			out = append(out, rule)
		}
	}
	return out
}

// Summary renders the rules that apply to a path, in load order.
func Summary(rules []Rule) string {
	var b strings.Builder
	for _, rule := range rules {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[%s] %s\n%s\n", rule.Source, rule.Name, strings.TrimSpace(rule.Body))
	}
	return b.String()
}

func readFile(source, path string) (Rule, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Rule{}, err
	}
	body := string(raw)
	var globs []string
	if strings.HasPrefix(body, "---\n") {
		scanner := bufio.NewScanner(strings.NewReader(body))
		scanner.Scan()
		var front []string
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == "---" {
				break
			}
			front = append(front, scanner.Text())
		}
		var meta struct {
			Globs []string `yaml:"globs"`
		}
		if err := yaml.Unmarshal([]byte(strings.Join(front, "\n")), &meta); err != nil {
			return Rule{}, err
		}
		globs = meta.Globs
		if idx := strings.Index(body, "\n---\n"); idx >= 0 {
			body = body[idx+len("\n---\n"):]
		}
	}
	return Rule{
		Source: source,
		Name:   strings.TrimSuffix(filepath.Base(path), ".md"),
		Path:   path,
		Body:   body,
		Globs:  globs,
	}, nil
}

func matchAny(globs []string, rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, glob := range globs {
		ok, err := pathMatch(filepath.ToSlash(glob), rel)
		if err == nil && ok {
			return true
		}
	}
	return false
}

func pathMatch(pattern, name string) (bool, error) {
	if strings.Contains(pattern, "**") {
		pattern = strings.ReplaceAll(pattern, "**", "*")
	}
	return filepath.Match(pattern, name)
}
