// Package catalog provides offline model discovery. Catalog entries never authorize inference.
package catalog

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed sources.default.yaml
var defaults []byte

//go:embed snapshot.json
var embedded []byte

type Model struct {
	Provider, ID, Name, Family, EndpointHint, AdapterHint, Source, Updated string
	Context, MaxOutput                                                     int
	Input, Output                                                          []string
	Tools, StructuredOutput, Reasoning                                     *bool
	CostPerMillion                                                         map[string]float64
}
type Source struct {
	ID, Kind, URL, Path, Format string
	Enabled                     bool
	Priority                    int
	Timeout                     time.Duration
	MaxBytes                    int64 `yaml:"max_bytes"`
}
type Parser func([]byte) ([]Model, error)
type Options struct {
	SourcesPath, CachePath     string
	Offline, AllowLoopbackHTTP bool
	TTL                        time.Duration
	Parsers                    map[string]Parser
	Overrides                  []Model
}
type Status struct {
	SchemaVersion int       `json:"schema_version"`
	Origin        string    `json:"origin"`
	SourceID      string    `json:"source_id"`
	Format        string    `json:"format"`
	Digest        string    `json:"content_digest"`
	Warning       string    `json:"warning,omitempty"`
	FetchedAt     time.Time `json:"fetched_at"`
	RecordCount   int       `json:"record_count"`
}
type Catalog struct {
	mu             sync.RWMutex
	refresh        chan struct{}
	opts           Options
	sources        []Source
	parsers        map[string]Parser
	models         []Model
	status         Status
	etag, location string
	configWarning  string
}
type cached struct {
	Status         Status
	ETag, Location string
	Data           []byte
}

const maxSourceBytes = 64 << 20

type identities[V any] map[string]V

func (ids *identities[V]) UnmarshalJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, e := d.Token()
	if e != nil || tok != json.Delim('{') {
		return errors.New("expected identity map")
	}
	m := make(identities[V])
	for d.More() {
		k, e := d.Token()
		if e != nil {
			return e
		}
		key, ok := k.(string)
		if !ok {
			return errors.New("invalid identity")
		}
		if _, ok := m[key]; ok {
			return errors.New("duplicate catalog identity")
		}
		var v V
		if e = d.Decode(&v); e != nil {
			return e
		}
		m[key] = v
	}
	_, e = d.Token()
	*ids = m
	return e
}
func parseModelsDev(data []byte) ([]Model, error) {
	var providers identities[struct {
		ID, API, NPM string
		Models       identities[struct {
			ID, Name, Family string
			Updated          string `json:"last_updated"`
			Limit            struct{ Context, Output int }
			Modalities       struct{ Input, Output []string }
			Tools            *bool `json:"tool_call"`
			Structured       *bool `json:"structured_output"`
			Reasoning        *bool
			Cost             json.RawMessage
		}]
	}]
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, err
	}
	var out []Model
	for p, v := range providers {
		if p == "" || p != v.ID {
			return nil, errors.New("invalid provider identity")
		}
		for k, m := range v.Models {
			if k == "" || k != m.ID || m.Name == "" {
				return nil, errors.New("invalid model identity")
			}
			var costs map[string]json.RawMessage
			if len(m.Cost) > 0 {
				if err := json.Unmarshal(m.Cost, &costs); err != nil {
					return nil, err
				}
			}
			prices := map[string]float64{}
			for _, name := range []string{"input", "output", "cache_read", "cache_write"} {
				if raw, ok := costs[name]; ok {
					var n float64
					if err := json.Unmarshal(raw, &n); err != nil {
						return nil, err
					}
					prices[name] = n
				}
			}
			out = append(out, Model{Provider: p, ID: k, Name: m.Name, Family: m.Family, Updated: m.Updated, EndpointHint: v.API, AdapterHint: v.NPM, Context: m.Limit.Context, MaxOutput: m.Limit.Output, Input: m.Modalities.Input, Output: m.Modalities.Output, Tools: m.Tools, StructuredOutput: m.Structured, Reasoning: m.Reasoning, CostPerMillion: prices})
		}
	}
	return out, nil
}

func readBounded(r io.Reader, n int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, n+1))
	if int64(len(b)) > n {
		return nil, errors.New("catalog exceeds limit")
	}
	return b, e
}
func readFile(path string, n int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return readBounded(f, n)
}
func sources(raw []byte, parsers map[string]Parser, local bool) ([]Source, error) {
	var doc struct {
		Version int
		Sources []Source
	}
	d := yaml.NewDecoder(bytes.NewReader(raw))
	d.KnownFields(true)
	if e := d.Decode(&doc); e != nil {
		return nil, e
	}
	if doc.Version != 1 {
		return nil, errors.New("unsupported source configuration version")
	}
	seen := map[string]bool{}
	for i := range doc.Sources {
		s := &doc.Sources[i]
		if !s.Enabled {
			continue
		}
		if s.MaxBytes == 0 {
			s.MaxBytes = 16 << 20
		}
		if s.Timeout == 0 {
			s.Timeout = 8 * time.Second
		}
		if s.ID == "" || seen[s.ID] || parsers[s.Format] == nil || s.MaxBytes < 1 || s.MaxBytes > maxSourceBytes || s.Timeout <= 0 {
			return nil, errors.New("invalid catalog source")
		}
		seen[s.ID] = true
		switch s.Kind {
		case "file":
			if !filepath.IsAbs(s.Path) {
				return nil, errors.New("expected absolute catalog path")
			}
		case "http":
			u, e := url.Parse(s.URL)
			if e != nil || (u.Scheme != "https" && !(local && u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback())) || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return nil, errors.New("expected credential-free HTTPS URL")
			}
		default:
			return nil, errors.New("unknown source kind")
		}
	}
	slices.SortStableFunc(doc.Sources, func(a, b Source) int { return cmp.Compare(b.Priority, a.Priority) })
	return doc.Sources, nil
}

// Open returns immediately from disk. Call Refresh in the caller's goroutine of choice.
func Open(opts Options) (*Catalog, error) {
	opts.Overrides = cloneModels(opts.Overrides)
	c := &Catalog{opts: opts, refresh: make(chan struct{}, 1), parsers: map[string]Parser{"modelsdev-provider-v1": parseModelsDev}}
	for name, p := range opts.Parsers {
		if p == nil || c.parsers[name] != nil {
			return nil, errors.New("invalid parser registration")
		}
		c.parsers[name] = p
	}
	var err error
	c.sources, err = sources(defaults, c.parsers, false)
	if err != nil {
		return nil, err
	}
	if opts.SourcesPath != "" {
		b, e := readFile(opts.SourcesPath, 1<<20)
		if e == nil {
			var ss []Source
			ss, e = sources(b, c.parsers, opts.AllowLoopbackHTTP)
			if e == nil {
				c.sources = ss
			}
		}
		if e != nil {
			c.configWarning = e.Error()
		}
	}
	base := cached{Status: Status{SchemaVersion: 1, Origin: "embedded", SourceID: "embedded", Format: "modelsdev-provider-v1"}, Data: embedded}
	if err := c.install(base, false); err != nil {
		return nil, err
	}
	if opts.CachePath != "" {
		raw, e := readFile(opts.CachePath, 2*maxSourceBytes)
		if e == nil {
			var v cached
			e = json.Unmarshal(raw, &v)
			if e == nil {
				v.Status.Origin = "cached"
				e = c.install(v, true)
			}
		}
		if e != nil && !os.IsNotExist(e) {
			c.status.Warning = e.Error()
		}
	}
	if c.configWarning != "" {
		c.status.Warning = c.configWarning
	}
	return c, nil
}
func (c *Catalog) install(v cached, verify bool) error {
	p := c.parsers[v.Status.Format]
	if p == nil || v.Status.SchemaVersion != 1 {
		return errors.New("unsupported catalog cache format")
	}
	models, e := p(v.Data)
	if e != nil {
		return e
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(v.Data))
	if len(models) == 0 || verify && (digest != v.Status.Digest || v.Status.RecordCount != len(models)) {
		return errors.New("invalid catalog digest or count")
	}
	v.Status.RecordCount = len(models)
	v.Status.Digest = digest
	type key struct{ provider, model string }
	byID := map[key]Model{}
	for _, m := range models {
		k := key{m.Provider, m.ID}
		if m.Provider == "" || m.ID == "" || byID[k].ID != "" {
			return errors.New("duplicate or empty model identity")
		}
		m.Source = v.Status.SourceID
		byID[k] = m
	}
	for _, m := range c.opts.Overrides {
		if m.Provider == "" || m.ID == "" {
			return errors.New("invalid model override")
		}
		m.Source = "user"
		byID[key{m.Provider, m.ID}] = m
	}
	out := make([]Model, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Model) int {
		if a.Provider != b.Provider {
			return strings.Compare(a.Provider, b.Provider)
		}
		return strings.Compare(a.ID, b.ID)
	})
	if (v.Status.Origin == "online" || v.Status.Origin == "file") && c.opts.CachePath != "" {
		b, e := json.Marshal(v)
		if e != nil {
			return e
		}
		if e = atomicSave(c.opts.CachePath, b); e != nil {
			return e
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = v.Status
	c.status.Warning = c.configWarning
	c.models = out
	c.etag = v.ETag
	c.location = v.Location
	return nil
}
func atomicSave(path string, b []byte) error {
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".catalog-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func (c *Catalog) Status() Status { c.mu.RLock(); defer c.mu.RUnlock(); return c.status }
func (c *Catalog) List() []Model {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneModels(c.models)
}
func (c *Catalog) Refresh(ctx context.Context) (Status, error) {
	select {
	case c.refresh <- struct{}{}:
		defer func() { <-c.refresh }()
	case <-ctx.Done():
		return c.Status(), ctx.Err()
	}
	st := c.Status()
	if c.opts.TTL > 0 && time.Since(st.FetchedAt) < c.opts.TTL {
		return st, nil
	}
	var errs []error
	for _, s := range c.sources {
		if !s.Enabled || c.opts.Offline && s.Kind == "http" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return c.Status(), err
		}
		err := c.load(ctx, s)
		if errors.Is(err, errTransient) {
			select {
			case <-ctx.Done():
				return c.Status(), ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
			err = c.load(ctx, s)
		}
		if err == nil {
			return c.Status(), nil
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", s.ID, err))
		}
	}
	err := errors.Join(errs...)
	if err != nil {
		c.mu.Lock()
		c.status.Warning = err.Error()
		c.mu.Unlock()
	}
	return c.Status(), err
}

var errTransient = errors.New("temporary catalog failure")

func (c *Catalog) load(ctx context.Context, s Source) error {
	var b []byte
	var err error
	etag := ""
	origin := "online"
	if s.Kind == "file" {
		origin = "file"
		b, err = readFile(s.Path, s.MaxBytes)
	} else {
		req, e := http.NewRequestWithContext(ctx, "GET", s.URL, nil)
		if e != nil {
			return e
		}
		if c.location == s.URL && c.Status().Format == s.Format && c.Status().SourceID == s.ID && c.etag != "" {
			req.Header.Set("If-None-Match", c.etag)
		}
		client := http.Client{Timeout: s.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, e := client.Do(req)
		if e != nil {
			return e
		}
		defer res.Body.Close()
		if res.StatusCode == 304 && req.Header.Get("If-None-Match") != "" {
			c.mu.Lock()
			c.status.Origin = "online"
			c.status.FetchedAt = time.Now().UTC()
			c.status.Warning = c.configWarning
			c.mu.Unlock()
			return nil
		}
		if res.StatusCode == 429 || res.StatusCode >= 500 {
			return fmt.Errorf("%w: HTTP %d", errTransient, res.StatusCode)
		}
		if res.StatusCode != 200 {
			return fmt.Errorf("HTTP %d", res.StatusCode)
		}
		etag = res.Header.Get("ETag")
		b, err = readBounded(res.Body, s.MaxBytes)
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return c.install(cached{Status: Status{SchemaVersion: 1, Origin: origin, FetchedAt: time.Now().UTC(), SourceID: s.ID, Format: s.Format}, Data: b, ETag: etag, Location: s.URL}, false)
}

func cloneModels(models []Model) []Model {
	out := slices.Clone(models)
	for i := range out {
		m := &out[i]
		m.Input = slices.Clone(m.Input)
		m.Output = slices.Clone(m.Output)
		m.CostPerMillion = maps.Clone(m.CostPerMillion)
		m.Tools = cloneBool(m.Tools)
		m.Reasoning = cloneBool(m.Reasoning)
		m.StructuredOutput = cloneBool(m.StructuredOutput)
	}
	return out
}
func cloneBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	b := *v
	return &b
}

// Snapshot returns one consistent version of the catalog and its diagnostics.
func (c *Catalog) Snapshot() (Status, []Model) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status, cloneModels(c.models)
}
