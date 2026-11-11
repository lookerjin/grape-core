package model

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

// OAuthProvider binds a registered product client to its authorized inference service.
type OAuthProvider struct {
	provider        Provider
	config          oauth2.Config
	requiredScope   string
	identityURL     string
	issuer, jwksURL string
	verifier        *oidc.IDTokenVerifier
}

func HuggingFace(clientID, clientSecret string) OAuthProvider {
	return OAuthProvider{
		provider: Provider{ID: "huggingface-router", Endpoint: "https://router.huggingface.co/v1", Protocol: Chat},
		config: oauth2.Config{
			ClientID: clientID, ClientSecret: clientSecret, Scopes: []string{"openid", "inference-api"},
			Endpoint: oauth2.Endpoint{
				AuthURL:       "https://huggingface.co/oauth/authorize",
				TokenURL:      "https://huggingface.co/oauth/token",
				DeviceAuthURL: "https://huggingface.co/oauth/device",
			},
		},
		issuer: "https://huggingface.co", jwksURL: "https://huggingface.co/oauth/jwks",
		identityURL: "https://huggingface.co/oauth/userinfo", requiredScope: "inference-api",
	}
}
func (p OAuthProvider) validate(local bool) error {
	if (p.issuer != "" || p.jwksURL != "" || hasScope(strings.Join(p.config.Scopes, " "), "openid")) && (!validEndpoint(p.issuer, local) || !validEndpoint(p.jwksURL, local)) {
		return errors.New("openid scope requires a trusted issuer and signing keys endpoint")
	}
	if p.provider.ID == "" || p.provider.Protocol != Chat || p.config.ClientID == "" || p.requiredScope == "" || !hasScope(strings.Join(p.config.Scopes, " "), p.requiredScope) {
		return errors.New("invalid OAuth registration or requested scope")
	}
	endpoints := []string{p.provider.Endpoint, p.identityURL, p.config.Endpoint.AuthURL, p.config.Endpoint.TokenURL}
	if p.config.Endpoint.DeviceAuthURL != "" {
		endpoints = append(endpoints, p.config.Endpoint.DeviceAuthURL)
	}
	for _, endpoint := range endpoints {
		if !validEndpoint(endpoint, local) {
			return errors.New("invalid OAuth endpoint")
		}
	}
	return nil
}
func (p OAuthProvider) registration() string {
	b, _ := json.Marshal([]string{p.provider.ID, p.provider.Endpoint, p.config.ClientID, p.config.Endpoint.AuthURL, p.config.Endpoint.TokenURL, p.identityURL, p.issuer, p.jwksURL, p.requiredScope})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

type credential struct {
	Registration string
	Token        *oauth2.Token
	Reauth       bool
}

func authContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 20 * time.Second, CheckRedirect: rejectRedirect})
}
func oauthError(ctx context.Context, e error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var re *oauth2.RetrieveError
	if errors.As(e, &re) {
		switch re.ErrorCode {
		case "invalid_grant", "access_denied", "expired_token":
			return ErrReauth
		}
	}
	return ErrCredentialUnavailable
}
func (r *Registry) saveLogin(ctx context.Context, p OAuthProvider, label, nonce string, t *oauth2.Token) (Connection, error) {
	if t == nil || t.AccessToken == "" || !strings.EqualFold(t.Type(), "Bearer") {
		return Connection{}, ErrReauth
	}
	if granted, ok := t.Extra("scope").(string); ok && !hasScope(granted, p.requiredScope) {
		return Connection{}, ErrReauth
	}
	raw, e := json.Marshal(credential{Registration: p.registration(), Token: t})
	if e != nil {
		return Connection{}, ErrCredentialUnavailable
	}
	q, err := http.NewRequestWithContext(ctx, "GET", p.identityURL, nil)
	if err != nil {
		return Connection{}, err
	}
	q.Header.Set("Authorization", "Bearer "+t.AccessToken)
	res, err := (&http.Client{Timeout: 15 * time.Second, CheckRedirect: rejectRedirect}).Do(q)
	if err != nil {
		return Connection{}, ErrReauth
	}
	defer res.Body.Close()
	var identity struct{ Sub string }
	b, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if res.StatusCode != 200 || err != nil || len(b) > 65536 || json.Unmarshal(b, &identity) != nil || identity.Sub == "" {
		return Connection{}, ErrReauth
	}
	if p.verifier != nil {
		rawID, _ := t.Extra("id_token").(string)
		identityToken, err := p.verifier.Verify(ctx, rawID)
		if err != nil || identityToken.Subject != identity.Sub || (nonce != "" && subtle.ConstantTimeCompare([]byte(nonce), []byte(identityToken.Nonce)) != 1) {
			return Connection{}, ErrReauth
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback()
	c, _, err := scan(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM model_connections WHERE app=? AND user=? AND provider=? AND registration=? AND subject=?", r.scope.App, r.scope.User, p.provider.ID, p.registration(), identity.Sub))
	if errors.Is(err, ErrNotConfigured) {
		c = Connection{ID: uuid.NewString(), Provider: p.provider, Label: label, Method: "oauth"}
		_, err = tx.ExecContext(ctx, `INSERT INTO model_connections(app,user,id,label,provider,endpoint,protocol,method,env,registration,subject) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, r.scope.App, r.scope.User, c.ID, label, p.provider.ID, p.provider.Endpoint, p.provider.Protocol, "oauth", "", p.registration(), identity.Sub)
	}
	if err != nil {
		return Connection{}, err
	}
	if r.store.Set(r.secretID(c.ID), string(raw)) != nil {
		return Connection{}, ErrCredentialUnavailable
	}
	if err = tx.Commit(); err != nil {
		return Connection{}, err
	}
	return c, nil
}
func hasScope(s, want string) bool {
	for _, v := range strings.Fields(s) {
		if v == want {
			return true
		}
	}
	return false
}

// Serialize token reads, refresh and secret replacement across registries sharing the metadata database.
func (r *Registry) accessToken(ctx context.Context, c Connection) (string, error) {
	p, ok := r.oauth[c.Provider.ID]
	if !ok || p.provider != c.Provider {
		return "", ErrReauth
	}
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	var disabled bool
	if e = tx.QueryRowContext(ctx, "SELECT disabled FROM model_connections WHERE id=? AND app=? AND user=?", c.ID, r.scope.App, r.scope.User).Scan(&disabled); e != nil || disabled {
		return "", ErrReauth
	}
	key := r.secretID(c.ID)
	raw, e := r.store.Get(key)
	if e != nil {
		return "", ErrCredentialUnavailable
	}
	var cred credential
	if json.Unmarshal([]byte(raw), &cred) != nil || cred.Registration != p.registration() || cred.Reauth || cred.Token == nil {
		return "", ErrReauth
	}
	if cred.Token.Valid() {
		return cred.Token.AccessToken, nil
	}
	if cred.Token.RefreshToken == "" {
		return "", ErrReauth
	}
	next, e := p.config.TokenSource(authContext(ctx), cred.Token).Token()
	if e != nil && !errors.Is(oauthError(ctx, e), ErrReauth) {
		return "", oauthError(ctx, e)
	}
	valid := next != nil && next.AccessToken != "" && strings.EqualFold(next.Type(), "Bearer")
	if valid {
		granted, reported := next.Extra("scope").(string)
		valid = !reported || hasScope(granted, p.requiredScope)
	}
	if e != nil || !valid {
		cred.Reauth = true
		cred.Token = nil
		b, _ := json.Marshal(cred)
		if r.store.Set(key, string(b)) != nil {
			return "", ErrCredentialUnavailable
		}
		return "", ErrReauth
	}
	cred.Token = next
	b, e := json.Marshal(cred)
	if e != nil || r.store.Set(key, string(b)) != nil {
		return "", ErrCredentialUnavailable
	}
	return next.AccessToken, nil
}

type Challenge struct {
	URL, UserCode string
	Expires       time.Time
}
type Login struct {
	nonce     string
	provider  OAuthProvider
	Challenge Challenge
	registry  *Registry
	cfg       oauth2.Config
	verifier  string
	code      chan string
	device    *oauth2.DeviceAuthResponse
	ctx       context.Context
	cancel    context.CancelFunc
	used      atomic.Bool
}

func (l *Login) Cancel() { l.cancel() }

// BeginLogin binds the registered loopback URI before returning the browser URL.
// Its parent context bounds both authorization and completion.
func (r *Registry) BeginLogin(parent context.Context, providerID, callback string) (*Login, error) {
	p, ok := r.oauth[providerID]
	if !ok {
		return nil, ErrNotConfigured
	}
	u, e := url.Parse(callback)
	if e != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return nil, errors.New("expected registered IPv4 loopback callback with port and path")
	}
	ln, e := net.Listen("tcp4", u.Host)
	if e != nil {
		return nil, e
	}
	u.Host = ln.Addr().String()
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	end, _ := ctx.Deadline()
	cfg := p.config
	cfg.RedirectURL = u.String()
	verifier, state := oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	authOpts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}
	nonce := ""
	if p.verifier != nil {
		nonce = oauth2.GenerateVerifier()
		authOpts = append(authOpts, oidc.Nonce(nonce))
	}
	l := &Login{nonce: nonce, provider: p, Challenge: Challenge{URL: cfg.AuthCodeURL(state, authOpts...), Expires: end}, registry: r, cfg: cfg, verifier: verifier, code: make(chan string, 1), ctx: ctx, cancel: cancel}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method != "GET" || q.Host != u.Host || q.URL.Path != u.Path || subtle.ConstantTimeCompare([]byte(q.URL.Query().Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid callback", 400)
			return
		}
		code := q.URL.Query().Get("code")
		if q.URL.Query().Get("error") != "" {
			code = ""
		}
		select {
		case l.code <- code:
		default:
		}
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, "Authorization received. Return to the application.")
	})}
	go func() { <-ctx.Done(); srv.Close(); ln.Close() }()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel()
		}
	}()
	return l, nil
}
func (r *Registry) BeginDevice(parent context.Context, providerID string) (*Login, error) {
	p, ok := r.oauth[providerID]
	if !ok || p.config.Endpoint.DeviceAuthURL == "" {
		return nil, ErrNotConfigured
	}
	cfg := p.config
	ctxStart, cancelStart := context.WithTimeout(parent, 20*time.Second)
	defer cancelStart()
	client := &http.Client{CheckRedirect: rejectRedirect, Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		return http.DefaultTransport.RoundTrip(req.WithContext(ctxStart))
	})}
	d, e := cfg.DeviceAuth(context.WithValue(ctxStart, oauth2.HTTPClient, client))
	if e != nil {
		return nil, oauthError(parent, e)
	}
	verification, err := url.Parse(d.VerificationURI)
	if err != nil || verification.Hostname() == "" || verification.User != nil || (verification.Scheme != "https" && !(r.local && verification.Scheme == "http" && net.ParseIP(verification.Hostname()).IsLoopback())) || d.DeviceCode == "" || d.UserCode == "" || d.Interval < 0 || d.Interval > 600 {
		return nil, errors.New("invalid device authorization challenge")
	}
	end := d.Expiry
	if end.IsZero() {
		end = time.Now().Add(10 * time.Minute)
	}
	ctx, cancel := context.WithDeadline(parent, end)
	return &Login{provider: p, Challenge: Challenge{URL: d.VerificationURI, UserCode: d.UserCode, Expires: end}, registry: r, cfg: cfg, device: d, ctx: ctx, cancel: cancel}, nil
}

// Complete exchanges the callback or polls the device grant once. Cancel interrupts either path.
func (l *Login) Complete(label string) (Connection, error) {
	if !l.used.CompareAndSwap(false, true) {
		return Connection{}, errors.New("login already consumed")
	}
	defer l.cancel()
	var t *oauth2.Token
	var e error
	ctx := authContext(l.ctx)
	if l.device != nil {
		t, e = l.cfg.DeviceAccessToken(ctx, l.device)
	} else {
		select {
		case <-ctx.Done():
			return Connection{}, ctx.Err()
		case code := <-l.code:
			if code == "" {
				return Connection{}, ErrReauth
			}
			t, e = l.cfg.Exchange(ctx, code, oauth2.VerifierOption(l.verifier))
		}
	}
	if e != nil {
		return Connection{}, oauthError(ctx, e)
	}
	return l.registry.saveLogin(ctx, l.provider, label, l.nonce, t)
}

// NewOAuthProvider binds a trusted OAuth registration, authenticated userinfo endpoint
// and one inference endpoint. The provider must support userinfo.sub and bearer chat requests.
func NewOAuthProvider(provider Provider, config oauth2.Config, userinfo, requiredScope string) OAuthProvider {
	return OAuthProvider{provider: provider, config: config, identityURL: userinfo, requiredScope: requiredScope}
}

// Formatting OAuth registrations and pending grants must not reveal client secrets or grant codes.
func (p OAuthProvider) String() string   { return p.provider.ID }
func (p OAuthProvider) GoString() string { return p.String() }
func (l *Login) String() string          { return "OAuth login" }
func (l *Login) GoString() string        { return l.String() }

// Invalidate only the credential rejected on this request. A concurrent refresh may
// already have installed a new token by the time an older request returns.
func (r *Registry) expireToken(ctx context.Context, c Connection, rejected string) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	key := r.secretID(c.ID)
	raw, err := r.store.Get(key)
	if err != nil {
		return
	}
	var v credential
	if json.Unmarshal([]byte(raw), &v) != nil || v.Token == nil || v.Token.AccessToken != rejected {
		return
	}
	v.Token.Expiry = time.Now().Add(-time.Minute)
	if v.Token.RefreshToken == "" {
		v.Reauth = true
		v.Token = nil
	}
	b, _ := json.Marshal(v)
	_ = r.store.Set(key, string(b))
}

// CredentialStatus reports local readiness without contacting an endpoint or rotating tokens.
func (r *Registry) CredentialStatus(ctx context.Context, id string) (string, error) {
	c, env, e := r.connection(ctx, r.db, id)
	if e != nil {
		return "", e
	}
	if c.Disabled {
		return "disabled", nil
	}
	if c.Method == "environment" {
		if os.Getenv(env) == "" {
			return "missing", nil
		}
		return "ready", nil
	}
	raw, e := r.store.Get(r.secretID(id))
	if e != nil || raw == "" {
		return "missing", nil
	}
	if c.Method != "oauth" {
		return "ready", nil
	}
	var v credential
	p, ok := r.oauth[c.Provider.ID]
	if !ok || json.Unmarshal([]byte(raw), &v) != nil || v.Registration != p.registration() || v.Token == nil || v.Reauth {
		return "reauth_required", nil
	}
	if v.Token.Valid() {
		return "ready", nil
	}
	if v.Token.RefreshToken != "" {
		return "refresh_required", nil
	}
	return "reauth_required", nil
}

// WithOIDC enables signature, issuer, audience, expiry and subject checks.
// Browser authorization also binds the ID token nonce to the pending login.
func (p OAuthProvider) WithOIDC(issuer, jwksURL string) OAuthProvider {
	p.issuer = issuer
	p.jwksURL = jwksURL
	return p
}
func (p *OAuthProvider) prepareVerifier() {
	if p.issuer != "" {
		p.verifier = oidc.NewVerifier(p.issuer, oidc.NewRemoteKeySet(authContext(context.Background()), p.jwksURL), &oidc.Config{ClientID: p.config.ClientID})
	}
}
