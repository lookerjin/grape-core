package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/oauth2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCredentialScopeAndRevokedGrant(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"sensitive-fixture"}`)
	}))
	defer server.Close()
	p := NewOAuthProvider(Provider{"test", server.URL, Chat}, oauth2.Config{ClientID: "client", Scopes: []string{"inference"}, Endpoint: oauth2.Endpoint{AuthURL: server.URL, TokenURL: server.URL, AuthStyle: oauth2.AuthStyleInParams}}, server.URL, "inference")
	o := Options{Scope: Scope{"app", "alice"}, Database: filepath.Join(t.TempDir(), "meta.db"), Store: &MemoryStore{}, AllowLoopbackHTTP: true, OAuth: []OAuthProvider{p}}
	a, e := Open(o)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	blob, _ := json.Marshal(credential{Registration: p.registration(), Token: &oauth2.Token{AccessToken: "old", RefreshToken: "rotated", Expiry: time.Now().Add(-time.Hour)}})
	c, e := a.add(ctx, p.provider, "label", "oauth", "", string(blob))
	if e != nil {
		t.Fatal(e)
	}
	ref := Ref{ConnectionID: c.ID, ModelID: "m"}
	o.Scope.User = "bob"
	b, e := Open(o)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if records, e := b.List(ctx); e != nil || len(records) != 0 {
		t.Fatal(records, e)
	}
	for _, e := range []error{b.SetEnabled(ctx, c.ID, false), b.SetDefault(ctx, ref), b.Remove(ctx, c.ID)} {
		if !errors.Is(e, ErrNotConfigured) {
			t.Fatal(e)
		}
	}
	if _, e = b.Resolve(ctx, a.Scope(), ref); !errors.Is(e, ErrNotConfigured) {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if _, e = a.Resolve(ctx, a.Scope(), ref); !errors.Is(e, ErrReauth) {
			t.Fatal(e)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("retried revoked grant %d times", n)
	}
	if st, e := a.CredentialStatus(ctx, c.ID); e != nil || st != "reauth_required" {
		t.Fatal(st, e)
	}
	if e = a.Remove(ctx, c.ID); e != nil {
		t.Fatal(e)
	}
}
