package coding

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Resolve returns a cleaned absolute workspace directory.
func Resolve(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("workspace is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory")
	}
	return resolved, nil
}

// Within checks that rel stays inside root, including through symlinks.
// The check opens the path with os.Root. Callers that read or write must use
// Read, Save, or Replace, which perform the operation on that same root.
func Within(root, rel string) (string, error) {
	root, err := Resolve(root)
	if err != nil {
		return "", err
	}
	rel, err = cleanRel(rel)
	if err != nil {
		return "", err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	if rel == "." {
		return root, nil
	}
	f, err := r.Open(rel)
	if err != nil {
		return "", err
	}
	f.Close()
	return filepath.Join(root, rel), nil
}

func cleanRel(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		return ".", nil
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path %q is outside the workspace", rel)
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return cleaned, nil
}

// Directory holds an open workspace for a sequence of confined file operations.
type Directory struct{ root *os.Root }

// OpenDirectory resolves and opens a directory. The caller owns Close.
func OpenDirectory(path string) (*Directory, error) {
	resolved, err := Resolve(path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, err
	}
	return &Directory{root: root}, nil
}

func (r *Directory) Close() error { return r.root.Close() }

// ReadLimit selects a line window and a byte cap. LineOffset is 1-based.
// LineLimit zero means the rest of the file. MaxBytes caps the returned
// text, not the bytes skipped to reach Offset. MaxLineBytes caps one
// physical line. Zero MaxLineBytes uses 1 MiB.
type ReadLimit struct {
	MaxBytes     int
	LineOffset   int
	LineLimit    int
	MaxLineBytes int
}

// Read returns the text of a workspace file. Symlinks that leave the
// workspace, absolute symlinks, and dangling symlinks are errors.
func Read(root, rel string) (string, error) {
	return ReadLimited(root, rel, ReadLimit{})
}

// ReadLimited reads at most lim.MaxBytes and optionally a line window.
// It does not load the rest of a larger file.
func ReadLimited(root, rel string, lim ReadLimit) (string, error) {
	r, err := OpenDirectory(root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	return r.ReadLimited(rel, lim)
}

func (r *Directory) ReadLimited(rel string, lim ReadLimit) (string, error) {
	rel, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	info, err := r.root.Stat(rel)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("is a directory")
	}
	f, err := r.root.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if lim.LineOffset > 1 || lim.LineLimit > 0 {
		return readLineWindow(f, rel, lim)
	}
	var reader io.Reader = f
	if lim.MaxBytes > 0 {
		reader = io.LimitReader(f, int64(lim.MaxBytes)+1)
	}
	b, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	if lim.MaxBytes > 0 && len(b) > lim.MaxBytes {
		return "", fmt.Errorf("file %s exceeds %d bytes", rel, lim.MaxBytes)
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("refusing to read non-UTF-8 file %s", rel)
	}
	return string(b), nil
}

func readLineWindow(r io.Reader, rel string, lim ReadLimit) (string, error) {
	br := bufio.NewReader(r)
	lineCap := lim.MaxLineBytes
	if lineCap <= 0 {
		lineCap = 1 << 20
	}
	start := 1
	if lim.LineOffset > 1 {
		start = lim.LineOffset
	}
	end := 0
	if lim.LineLimit > 0 {
		end = start + lim.LineLimit - 1
	}
	var b strings.Builder
	line := 0
	for {
		keep := false
		next := line + 1
		if next >= start && (end == 0 || next <= end) {
			keep = true
		}
		var text string
		var eof bool
		var err error
		if keep {
			text, eof, err = readCappedLine(br, lineCap)
		} else {
			eof, err = discardLine(br)
		}
		if err != nil {
			return "", fmt.Errorf("file %s line %d: %w", rel, next, err)
		}
		if !keep && eof && next == 1 && line == 0 {
			break
		}
		if keep && text == "" && eof && line == 0 {
			break
		}
		line++
		if line >= start && (end == 0 || line <= end) {
			if b.Len() > 0 {
				if lim.MaxBytes > 0 && b.Len()+1 > lim.MaxBytes {
					return "", fmt.Errorf("file %s output exceeds %d bytes", rel, lim.MaxBytes)
				}
				b.WriteByte('\n')
			}
			if lim.MaxBytes > 0 && b.Len()+len(text) > lim.MaxBytes {
				return "", fmt.Errorf("file %s output exceeds %d bytes", rel, lim.MaxBytes)
			}
			b.WriteString(text)
		}
		if end > 0 && line >= end {
			break
		}
		if eof {
			break
		}
	}
	if !utf8.ValidString(b.String()) {
		return "", fmt.Errorf("refusing to read non-UTF-8 file %s", rel)
	}
	return b.String(), nil
}

// readCappedLine reads one line without bufio.Scanner's 64KB token limit.
func readCappedLine(r *bufio.Reader, max int) (string, bool, error) {
	var b strings.Builder
	for {
		frag, err := r.ReadSlice('\n')
		if len(frag) > 0 {
			if b.Len()+len(frag) > max+1 {
				return "", false, fmt.Errorf("exceeds %d bytes", max)
			}
			b.Write(frag)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		text := strings.TrimRight(b.String(), "\r\n")
		if err == io.EOF {
			return text, true, nil
		}
		if err != nil {
			return "", false, err
		}
		return text, false, nil
	}
}

func discardLine(r *bufio.Reader) (bool, error) {
	for {
		_, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	}
}

// List returns names of entries in a workspace directory.
func List(root, rel string) ([]string, error) {
	r, err := OpenDirectory(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.List(rel)
}

func (r *Directory) List(rel string) ([]string, error) {
	rel, err := cleanRel(rel)
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(r.root.FS(), rel)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out, nil
}

// Hit is one substring match.
type Hit struct {
	Path string
	Line int
	Text string
}

// SearchLimit caps how much of each file is read and how many hits are
// returned. Zero MaxBytes skips files that cannot be fully read under the
// caller's cap by using no cap. Zero MaxHits means no hit cap.
type SearchLimit struct {
	MaxBytes int
	MaxHits  int
}

// Search walks the workspace for a literal substring. It skips directories
// named .git and does not follow symlinks that leave the workspace.
func Search(root, substr string) ([]Hit, error) {
	hits, _, err := SearchLimited(root, ".", substr, SearchLimit{})
	return hits, err
}

// SearchLimited searches under rel. rel may be a file or a directory.
// skipped is how many files were left out because they exceed MaxBytes.
func SearchLimited(root, rel, substr string, lim SearchLimit) ([]Hit, int, error) {
	r, err := OpenDirectory(root)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	return r.SearchLimited(rel, substr, lim)
}

func (r *Directory) SearchLimited(rel, substr string, lim SearchLimit) ([]Hit, int, error) {
	if substr == "" {
		return nil, 0, fmt.Errorf("empty search")
	}
	rel, err := cleanRel(rel)
	if err != nil {
		return nil, 0, err
	}
	var hits []Hit
	var skipped int
	err = fs.WalkDir(r.root.FS(), rel, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if lim.MaxBytes > 0 && info.Size() > int64(lim.MaxBytes) {
			skipped++
			return nil
		}
		b, err := r.root.ReadFile(path)
		if err != nil || !utf8.Valid(b) {
			return nil
		}
		if lim.MaxBytes > 0 && len(b) > lim.MaxBytes {
			skipped++
			return nil
		}
		text := string(b)
		if !strings.Contains(text, substr) {
			return nil
		}
		line := 1
		for _, chunk := range strings.Split(text, "\n") {
			if strings.Contains(chunk, substr) {
				hits = append(hits, Hit{Path: path, Line: line, Text: chunk})
				if lim.MaxHits > 0 && len(hits) >= lim.MaxHits {
					return fs.SkipAll
				}
			}
			line++
		}
		return nil
	})
	if err == fs.SkipAll {
		err = nil
	}
	return hits, skipped, err
}

// Replace replaces old with new in a workspace file and keeps the file mode.
// n is the maximum number of replacements. A non-positive n means one.
// The read and write both go through os.Root, so a symlink cannot retarget
// the write outside the workspace between the two steps.
func Replace(root, rel, old, new string, n int) error {
	r, err := OpenDirectory(root)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.Replace(rel, old, new, n)
}

func (r *Directory) Replace(rel, old, new string, n int) error {
	if old == "" {
		return fmt.Errorf("empty old string")
	}
	rel, err := cleanRel(rel)
	if err != nil {
		return err
	}
	info, err := r.root.Stat(rel)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("is a directory")
	}
	b, err := r.root.ReadFile(rel)
	if err != nil {
		return err
	}
	if !utf8.Valid(b) {
		return fmt.Errorf("refusing to edit non-UTF-8 file %s", rel)
	}
	if n <= 0 {
		n = 1
	}
	text := string(b)
	if strings.Count(text, old) == 0 {
		return fmt.Errorf("old string not found")
	}
	updated := strings.Replace(text, old, new, n)
	mode := info.Mode().Perm()
	if err := r.root.WriteFile(rel, []byte(updated), mode); err != nil {
		return err
	}
	return r.root.Chmod(rel, mode)
}

// Save writes a UTF-8 file inside the workspace.
func Save(root, rel, contents string, overwrite bool) error {
	r, err := OpenDirectory(root)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.Save(rel, contents, overwrite)
}

func (r *Directory) Save(rel, contents string, overwrite bool) error {
	if !utf8.ValidString(contents) {
		return fmt.Errorf("refusing to write non-UTF-8 content")
	}
	rel, err := cleanRel(rel)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(rel); dir != "." {
		if err := r.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if !overwrite {
		if _, err := r.root.Lstat(rel); err == nil {
			return fmt.Errorf("file exists")
		}
	}
	return r.root.WriteFile(rel, []byte(contents), 0o644)
}

// Match lists files and directories under rel whose path or base name matches
// pattern. An empty pattern matches everything. Symlinks are not followed.
func Match(root, rel, pattern string) (files, dirs []string, err error) {
	r, err := OpenDirectory(root)
	if err != nil {
		return nil, nil, err
	}
	defer r.Close()
	return r.Match(rel, pattern)
}

func (r *Directory) Match(rel, pattern string) (files, dirs []string, err error) {
	rel, err = cleanRel(rel)
	if err != nil {
		return nil, nil, err
	}
	if _, err := fs.Stat(r.root.FS(), rel); err != nil {
		return nil, nil, err
	}
	err = fs.WalkDir(r.root.FS(), rel, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == rel && d.IsDir() {
			return nil
		}
		if pattern != "" {
			baseOK, _ := filepath.Match(pattern, d.Name())
			pathOK, _ := filepath.Match(pattern, path)
			if !baseOK && !pathOK {
				return nil
			}
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files, dirs, err
}
