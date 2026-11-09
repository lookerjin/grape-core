package skill

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Summary is the progressive-disclosure view of one skill.
type Summary struct {
	Name        string
	Description string
	Dir         string
	Root        string
}

// ErrConflict is returned when two roots define the same skill name.
var ErrConflict = fmt.Errorf("skill name conflict")

// Discover scans roots for SKILL.md files.
// policy is "error", "earliest", or "latest". Roots are scanned in order.
func Discover(policy string, roots ...string) ([]Summary, error) {
	if policy == "" {
		policy = "error"
	}
	var out []Summary
	index := map[string]int{}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		found, err := scan(abs)
		if err != nil {
			return nil, err
		}
		for _, summary := range found {
			pos, ok := index[summary.Name]
			if !ok {
				index[summary.Name] = len(out)
				out = append(out, summary)
				continue
			}
			switch policy {
			case "error":
				return nil, fmt.Errorf("%w: %s in %s and %s", ErrConflict, summary.Name, out[pos].Dir, summary.Dir)
			case "earliest":
				continue
			case "latest":
				out[pos] = summary
			default:
				return nil, fmt.Errorf("unknown skill conflict policy %q", policy)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func scan(root string) ([]Summary, error) {
	var out []Summary
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}
		summary, err := readSummary(filepath.Dir(path), root)
		if err != nil {
			return err
		}
		out = append(out, summary)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

func readSummary(dir, root string) (Summary, error) {
	f, err := os.Open(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Summary{}, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var front []string
	inFront := false
	seen := false
	for scanner.Scan() {
		line := scanner.Text()
		if !seen {
			if strings.TrimSpace(line) != "---" {
				break
			}
			seen = true
			inFront = true
			continue
		}
		if inFront && strings.TrimSpace(line) == "---" {
			break
		}
		front = append(front, line)
	}
	if err := scanner.Err(); err != nil {
		return Summary{}, err
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if len(front) > 0 {
		if err := yaml.Unmarshal([]byte(strings.Join(front, "\n")), &meta); err != nil {
			return Summary{}, err
		}
	}
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		name = filepath.Base(dir)
	}
	return Summary{
		Name:        name,
		Description: strings.TrimSpace(meta.Description),
		Dir:         dir,
		Root:        root,
	}, nil
}
