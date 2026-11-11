package model

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

func TestOIDCLoginChecksPendingNonceAndAccount(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/keys" {
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "key", Algorithm: "RS256", Use: "sig"}}})
			return
		}
		fmt.Fprint(w, `{"sub":"account"}`)
	}))
	defer s.Close()
	p := NewOAuthProvider(Provider{"fixture", s.URL, Chat}, oauth2.Config{ClientID: "app", Endpoint: oauth2.Endpoint{AuthURL: s.URL + "/auth", TokenURL: s.URL + "/token"}, Scopes: []string{"openid", "infer"}}, s.URL+"/me", "infer").WithOIDC(s.URL, s.URL+"/keys")
	r, err := Open(Options{Scope: Scope{"local", "user"}, Database: filepath.Join(t.TempDir(), "accounts.db"), Store: &MemoryStore{}, AllowLoopbackHTTP: true, OAuth: []OAuthProvider{p}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sub, nonce string
		accept     bool
	}{{"account", "other", false}, {"different", "pending", false}, {"account", "pending", true}} {
		claims, _ := json.Marshal(map[string]any{"iss": s.URL, "sub": tc.sub, "aud": "app", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": tc.nonce})
		sig, err := signer.Sign(claims)
		if err != nil {
			t.Fatal(err)
		}
		id, err := sig.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		token := (&oauth2.Token{AccessToken: "access", TokenType: "Bearer"}).WithExtra(map[string]any{"id_token": id, "scope": "openid infer"})
		_, err = r.saveLogin(context.Background(), r.oauth["fixture"], "fixture", "pending", token)
		if tc.accept && err != nil || !tc.accept && !errors.Is(err, ErrReauth) {
			t.Fatalf("unexpected login result %v", err)
		}
	}
}
