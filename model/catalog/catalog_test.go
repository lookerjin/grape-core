package catalog

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestRefreshAndReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sources.yaml")
	cache := filepath.Join(dir, "cache")
	var stage atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("unexpected credentials")
		}
		switch stage.Load() {
		case 0:
			w.Header().Set("ETag", `"v1"`)
			fmt.Fprint(w, `{"p":{"id":"p","models":{"a":{"id":"a","name":"A"}}}}`)
		case 1:
			if r.Header.Get("If-None-Match") != `"v1"` {
				t.Error("missing validator")
			}
			w.WriteHeader(304)
		default:
			fmt.Fprint(w, `{"p":{"id":"p","models":{"a":{"id":"a","name":"A"},"a":{"id":"a","name":"B"}}}}`)
		}
	}))
	defer s.Close()
	os.WriteFile(path, []byte("version: 1\nsources:\n  - id: site\n    enabled: true\n    kind: http\n    format: modelsdev-provider-v1\n    url: "+s.URL+"\n"), 0600)
	o := Options{SourcesPath: path, CachePath: cache, AllowLoopbackHTTP: true}
	c, e := Open(o)
	if e != nil || c.Status().Origin != "embedded" || len(c.List()) < 8 {
		t.Fatal(e)
	}
	for _, v := range []int32{0, 1, 2} {
		stage.Store(v)
		st, e := c.Refresh(context.Background())
		if (e != nil) != (v == 2) || st.RecordCount != 1 || c.List()[0].Name != "A" {
			t.Fatal(v, st, e)
		}
	}
	o.Offline = true
	c, e = Open(o)
	if e != nil || c.Status().Origin != "cached" || len(c.List()) != 1 {
		t.Fatal(e, c.Status())
	}
	os.WriteFile(path, []byte("broken"), 0600)
	c, e = Open(o)
	if e != nil || c.Status().Warning == "" || c.List()[0].ID != "a" {
		t.Fatal(e)
	}
}

func TestTrustedFileAndCustomParser(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "export")
	cfg := filepath.Join(dir, "sources.yaml")
	if e := os.WriteFile(src, []byte("vendor-stable-export"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(cfg, []byte("version: 1\nsources:\n  - id: vendor\n    kind: file\n    enabled: true\n    format: vendor-v2\n    path: "+src+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := Open(Options{Offline: true, SourcesPath: cfg, Parsers: map[string]Parser{"vendor-v2": func(b []byte) ([]Model, error) {
		return []Model{{Provider: "custom", ID: string(b), Name: "original"}}, nil
	}}, Overrides: []Model{{Provider: "custom", ID: "vendor-stable-export", Name: "chosen"}}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Refresh(context.Background()); e != nil {
		t.Fatal(e)
	}
	st, records := c.Snapshot()
	if st.Format != "vendor-v2" || len(records) != 1 || records[0].Name != "chosen" || records[0].Source != "user" {
		t.Fatal(st, records)
	}
}
