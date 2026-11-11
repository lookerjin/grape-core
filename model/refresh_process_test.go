package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"golang.org/x/oauth2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Shared backend for synthetic test tokens only. Production uses the OS keychain.
type fixtureStore string

func (s fixtureStore) path(k string) string {
	return filepath.Join(string(s), fmt.Sprintf("%x", sha256.Sum256([]byte(k))))
}
func (s fixtureStore) Get(k string) (string, error) {
	b, e := os.ReadFile(s.path(k))
	return string(b), e
}
func (s fixtureStore) Set(k, v string) error { return os.WriteFile(s.path(k), []byte(v), 0600) }
func (s fixtureStore) Delete(k string) error { return os.Remove(s.path(k)) }
func TestRefreshAcrossProcesses(t *testing.T) {
	child := os.Getenv("GRAPE_REFRESH_TEST_CHILD") == "1"
	var refresh atomic.Int32
	endpoint, dir := os.Getenv("GRAPE_REFRESH_TEST_URL"), os.Getenv("GRAPE_REFRESH_TEST_DIR")
	if !child {
		dir = t.TempDir()
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.FormValue("refresh_token") != "one-use" || refresh.Add(1) != 1 {
				http.Error(w, "already rotated", 400)
				return
			}
			time.Sleep(150 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"new","refresh_token":"rotated","token_type":"Bearer","expires_in":3600}`)
		}))
		defer s.Close()
		endpoint = s.URL
	}
	cfg := oauth2.Config{ClientID: "app", Scopes: []string{"infer"}, Endpoint: oauth2.Endpoint{AuthURL: endpoint, TokenURL: endpoint, AuthStyle: oauth2.AuthStyleInParams}}
	p := NewOAuthProvider(Provider{"fixture", endpoint, Chat}, cfg, endpoint, "infer")
	o := Options{Scope: Scope{"app", "u"}, Database: filepath.Join(dir, "database"), Store: fixtureStore(dir), AllowLoopbackHTTP: true, OAuth: []OAuthProvider{p}}
	r, e := Open(o)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	ctx := context.Background()
	if child {
		_, e = r.Resolve(ctx, o.Scope, Ref{ConnectionID: os.Getenv("GRAPE_REFRESH_TEST_ID"), ModelID: "m"})
		if e != nil {
			t.Fatal(e)
		}
		return
	}
	raw, _ := json.Marshal(credential{Registration: p.registration(), Token: &oauth2.Token{AccessToken: "old", RefreshToken: "one-use", Expiry: time.Now().Add(-time.Hour)}})
	c, e := r.add(ctx, p.provider, "test", "oauth", "", string(raw))
	if e != nil {
		t.Fatal(e)
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			cmd := exec.Command(exe, "-test.run=^TestRefreshAcrossProcesses$")
			cmd.Env = append(os.Environ(), "GRAPE_REFRESH_TEST_CHILD=1", "GRAPE_REFRESH_TEST_DIR="+dir, "GRAPE_REFRESH_TEST_URL="+endpoint, "GRAPE_REFRESH_TEST_ID="+c.ID)
			out, e := cmd.CombinedOutput()
			if e != nil {
				e = fmt.Errorf("refresh subprocess: %w %s", e, out)
			}
			results <- e
		}()
	}
	for range 2 {
		if e := <-results; e != nil {
			t.Error(e)
		}
	}
	if refresh.Load() != 1 {
		t.Fatalf("requests %d", refresh.Load())
	}
}
