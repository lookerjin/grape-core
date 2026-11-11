// Package model manages trusted product model connections independently of the agent engine.
package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/modelaccess"
	_ "github.com/mattn/go-sqlite3"
	"github.com/zalando/go-keyring"
)

var (
	ErrNotConfigured         = errors.New("model not configured")
	ErrCredentialUnavailable = errors.New("credential unavailable")
	ErrReauth                = errors.New("account authorization required")
	ErrAttachmentScope       = errors.New("file belongs to another model connection")
)

type Ref = agent.ModelRef

// Scope identifies a principal already authenticated by the embedding application.
type Scope struct{ App, User string }
type Protocol string

const (
	Chat     Protocol = "chat_completions"
	Messages Protocol = "anthropic_messages"
	Gemini   Protocol = "gemini_native"
)

// Provider is a trusted inference destination and wire protocol.
type Provider struct {
	ID, Endpoint string
	Protocol     Protocol
}
type Connection struct {
	ID, Label, Method string
	Provider          Provider
	Disabled          bool
	AllowBackground   bool
}

// Store holds opaque secrets. Set must replace one entry atomically.
type Store interface {
	Get(string) (string, error)
	Set(string, string) error
	Delete(string) error
}

// Keychain uses the operating system's credential service. Errors never trigger plaintext fallback.
type Keychain struct{}

func (Keychain) Get(k string) (string, error) { return keyring.Get("grape-core", k) }
func (Keychain) Set(k, v string) error        { return keyring.Set("grape-core", k, v) }
func (Keychain) Delete(k string) error {
	e := keyring.Delete("grape-core", k)
	if errors.Is(e, keyring.ErrNotFound) {
		return nil
	}
	return e
}

// MemoryStore keeps secrets only for the lifetime of this instance.
type MemoryStore struct{ values sync.Map }

func (m *MemoryStore) Get(k string) (string, error) {
	v, ok := m.values.Load(k)
	if !ok {
		return "", ErrCredentialUnavailable
	}
	return v.(string), nil
}
func (m *MemoryStore) Set(k, v string) error { m.values.Store(k, v); return nil }
func (m *MemoryStore) Delete(k string) error { m.values.Delete(k); return nil }

type Registry struct {
	scope Scope
	db    *sql.DB
	store Store
	local bool
	oauth map[string]OAuthProvider
}
type Options struct {
	Scope             Scope
	Database          string
	Store             Store
	AllowLoopbackHTTP bool
	OAuth             []OAuthProvider
}

// Open binds a registry to the already authenticated principal and application.
// Database, providers and credential references come exclusively from trusted host configuration.
func Open(o Options) (*Registry, error) {
	if o.Scope.User == "" || o.Scope.App == "" || !filepath.IsAbs(o.Database) {
		return nil, errors.New("registry requires app, user and absolute database path")
	}
	if e := os.MkdirAll(filepath.Dir(o.Database), 0700); e != nil {
		return nil, e
	}
	file, e := os.OpenFile(o.Database, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = file.Close(); e != nil {
		return nil, e
	}
	u := url.URL{Scheme: "file", Path: o.Database, RawQuery: "_busy_timeout=5000&_txlock=immediate"}
	db, e := sql.Open("sqlite3", u.String())
	if e != nil {
		return nil, e
	}
	_, e = db.Exec(`
 CREATE TABLE IF NOT EXISTS model_connections (
   id TEXT PRIMARY KEY, app TEXT NOT NULL, user TEXT NOT NULL,
   label TEXT, provider TEXT, endpoint TEXT, protocol TEXT, method TEXT, env TEXT,
   disabled INTEGER NOT NULL DEFAULT 0, background INTEGER NOT NULL DEFAULT 0,
   registration TEXT NOT NULL DEFAULT '', subject TEXT NOT NULL DEFAULT ''
 );
 CREATE TABLE IF NOT EXISTS model_defaults (
   app TEXT, user TEXT, connection TEXT, model TEXT, PRIMARY KEY(app,user)
 );
 CREATE TABLE IF NOT EXISTS model_observations (
   connection TEXT, model TEXT, observed TEXT, status TEXT, PRIMARY KEY(connection,model)
 );`)
	if e != nil {
		db.Close()
		return nil, e
	}
	if o.Store == nil {
		o.Store = Keychain{}
	}
	r := &Registry{scope: o.Scope, db: db, store: o.Store, local: o.AllowLoopbackHTTP}
	r.oauth = map[string]OAuthProvider{}
	for _, p := range o.OAuth {
		if err := p.validate(r.local); err != nil {
			db.Close()
			return nil, err
		}
		if _, ok := r.oauth[p.provider.ID]; ok {
			db.Close()
			return nil, errors.New("duplicate OAuth provider")
		}
		p.config.Scopes = append([]string(nil), p.config.Scopes...)
		p.prepareVerifier()
		r.oauth[p.provider.ID] = p
	}

	return r, nil
}
func (r *Registry) Scope() Scope { return r.scope }
func (r *Registry) Close() error { return r.db.Close() }
func validEndpoint(raw string, local bool) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return false
	}
	return u.Scheme == "https" || local && u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback()
}
func (r *Registry) secretID(id string) string {
	b, _ := json.Marshal([]string{r.scope.App, r.scope.User, id})
	return string(b)
}
func (r *Registry) add(ctx context.Context, p Provider, label, method, env, secret string) (Connection, error) {
	var c Connection
	if p.ID == "" || !validEndpoint(p.Endpoint, r.local) || (p.Protocol != Chat && p.Protocol != Messages && p.Protocol != Gemini) {
		return c, errors.New("invalid inference provider")
	}
	c = Connection{ID: uuid.NewString(), Provider: p, Label: label, Method: method}
	if secret != "" {
		if e := r.store.Set(r.secretID(c.ID), secret); e != nil {
			return Connection{}, ErrCredentialUnavailable
		}
	}
	_, e := r.db.ExecContext(ctx, "INSERT INTO model_connections(app,user,id,label,provider,endpoint,protocol,method,env) VALUES(?,?,?,?,?,?,?,?,?)", r.scope.App, r.scope.User, c.ID, label, p.ID, p.Endpoint, p.Protocol, method, env)
	if e != nil && secret != "" {
		_ = r.store.Delete(r.secretID(c.ID))
	}
	return c, e
}
func (r *Registry) AddKey(ctx context.Context, p Provider, label, key string) (Connection, error) {
	if strings.TrimSpace(key) == "" {
		return Connection{}, ErrCredentialUnavailable
	}
	return r.add(ctx, p, label, "api_key", "", key)
}
func (r *Registry) AddEnv(ctx context.Context, p Provider, label, env string) (Connection, error) {
	if env == "" || strings.ContainsAny(env, "=\x00") {
		return Connection{}, ErrCredentialUnavailable
	}
	return r.add(ctx, p, label, "environment", env, "")
}

const columns = "id,label,provider,endpoint,protocol,method,disabled,background,env"

type scanner interface{ Scan(...any) error }

func scan(s scanner) (Connection, string, error) {
	var c Connection
	var env string
	e := s.Scan(&c.ID, &c.Label, &c.Provider.ID, &c.Provider.Endpoint, &c.Provider.Protocol, &c.Method, &c.Disabled, &c.AllowBackground, &env)
	if errors.Is(e, sql.ErrNoRows) {
		e = ErrNotConfigured
	}
	return c, env, e
}
func (r *Registry) List(ctx context.Context) ([]Connection, error) {
	rows, e := r.db.QueryContext(ctx, "SELECT "+columns+" FROM model_connections WHERE app=? AND user=? ORDER BY label,id", r.scope.App, r.scope.User)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		c, _, e := scan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (r *Registry) SetEnabled(ctx context.Context, id string, enabled bool) error {
	res, e := r.db.ExecContext(ctx, "UPDATE model_connections SET disabled=? WHERE app=? AND user=? AND id=?", !enabled, r.scope.App, r.scope.User, id)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if n == 0 {
		return ErrNotConfigured
	}
	return e
}

type connectionReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (r *Registry) connection(ctx context.Context, db connectionReader, id string) (Connection, string, error) {
	return scan(db.QueryRowContext(ctx, "SELECT "+columns+" FROM model_connections WHERE app=? AND user=? AND id=?", r.scope.App, r.scope.User, id))
}

func (r *Registry) Remove(ctx context.Context, id string) error {
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	c, _, e := r.connection(ctx, tx, id)
	if e != nil {
		return e
	}
	if c.Method != "environment" {
		if e = r.store.Delete(r.secretID(id)); e != nil {
			return ErrCredentialUnavailable
		}
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM model_observations WHERE connection=?", id); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM model_defaults WHERE connection=?", id); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM model_connections WHERE id=?", id); e != nil {
		return e
	}
	return tx.Commit()
}

// Binding holds the selected immutable connection. Secrets are omitted from formatting and serialization.
type Binding struct {
	Ref       Ref
	Provider  Provider
	key       string
	transport http.RoundTripper
}

func (b Binding) String() string   { return b.Provider.ID + "/" + b.Ref.ModelID }
func (b Binding) GoString() string { return b.String() }

// APIKey is for a trusted native model constructor only.
func (b Binding) NativeAPIKey(modelaccess.Native) string { return b.key }
func (b Binding) Client(modelaccess.Native) *http.Client {
	return &http.Client{Transport: b.transport, CheckRedirect: rejectRedirect}
}
func rejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(q *http.Request) (*http.Response, error) { return f(q) }
func (r *Registry) Resolve(ctx context.Context, scope Scope, ref Ref) (Binding, error) {
	if ref.ModelID == "" {
		return Binding{}, ErrNotConfigured
	}
	return r.resolve(ctx, scope, ref, false)
}
func (r *Registry) ResolveBackground(ctx context.Context, scope Scope, ref Ref) (Binding, error) {
	if ref.ModelID == "" {
		return Binding{}, ErrNotConfigured
	}
	return r.resolve(ctx, scope, ref, true)
}
func (r *Registry) resolve(ctx context.Context, scope Scope, ref Ref, background bool) (Binding, error) {
	var b Binding
	if scope != r.scope || ref.ConnectionID == "" {
		return b, ErrNotConfigured
	}
	c, env, e := r.connection(ctx, r.db, ref.ConnectionID)
	if e != nil {
		return b, e
	}
	if background && c.Method == "oauth" && !c.AllowBackground {
		return b, agent.ErrPermissionDenied
	}
	if c.Disabled {
		return b, ErrNotConfigured
	}
	var key string
	switch c.Method {
	case "environment":
		key = os.Getenv(env)
	case "api_key":
		key, e = r.store.Get(r.secretID(c.ID))
	case "oauth":
		key, e = r.accessToken(ctx, c)
	default:
		return b, ErrNotConfigured
	}
	if e != nil {
		if errors.Is(e, ErrReauth) || ctx.Err() != nil {
			return b, e
		}
		return b, ErrCredentialUnavailable
	}
	if key == "" {
		return b, ErrCredentialUnavailable
	}
	u, parseErr := url.Parse(c.Provider.Endpoint)
	if parseErr != nil || !validEndpoint(c.Provider.Endpoint, r.local) {
		return b, errors.New("invalid stored provider endpoint")
	}
	nativeKey := key
	if c.Method == "oauth" {
		nativeKey = ""
	}
	return Binding{Ref: ref, Provider: c.Provider, key: nativeKey, transport: roundTrip(func(q *http.Request) (*http.Response, error) {
		if q.URL.Scheme != u.Scheme || !strings.EqualFold(q.URL.Host, u.Host) || q.URL.User != nil {
			return nil, errors.New("credential target mismatch")
		}
		q = q.Clone(q.Context())
		if c.Method == "oauth" {
			base := strings.TrimRight(u.EscapedPath(), "/")
			p := q.URL.EscapedPath()
			adapter := r.oauth[c.Provider.ID]
			routes := adapter.inferenceRoutes()
			switch {
			case q.Method == "POST" && p == base+adapter.nativeGeneratePath():
				q.URL.Path = strings.TrimRight(u.Path, "/") + routes.Generate
				q.URL.RawPath = ""
			case q.Method == "GET" && p == base+routes.Models:
			default:
				return nil, errors.New("request outside OAuth inference scope")
			}
		}
		for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie", "OpenAI-Project", "OpenAI-Organization"} {
			q.Header.Del(name)
		}
		switch c.Provider.Protocol {
		case Chat:
			q.Header.Set("Authorization", "Bearer "+key)
		case Messages:
			if c.Method != "oauth" {
				q.Header.Set("X-Api-Key", key)
			}
		case Gemini:
			q.Header.Set("X-Goog-Api-Key", key)
		}
		if c.Method == "oauth" {
			fresh, e := r.accessToken(q.Context(), c)
			if e != nil {
				return nil, e
			}
			q.Header.Set("Authorization", "Bearer "+fresh)
		}
		res, e := http.DefaultTransport.RoundTrip(q)
		if e != nil {
			if q.Context().Err() != nil {
				return nil, q.Context().Err()
			}
			return nil, errors.New("model transport unavailable")
		}
		// The upstream SDK can include remote error bodies in session events. Keep only status.
		res.Header.Del("X-Grape-Model-Error")
		if res.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
			res.Body.Close()
			var reason struct{ Error struct{ Code string } }
			_ = json.Unmarshal(raw, &reason)
			class := httpErrorClass(res.StatusCode, reason.Error.Code)
			res.Body = http.NoBody
			res.ContentLength = 0
			res.Header = make(http.Header)
			res.Header.Set("X-Grape-Model-Error", class)
			res.Status = fmt.Sprintf("%d %s", res.StatusCode, http.StatusText(res.StatusCode))
			if c.Method == "oauth" && res.StatusCode == 401 {
				r.expireToken(q.Context(), c, strings.TrimPrefix(q.Header.Get("Authorization"), "Bearer "))
			}
		}
		return res, nil
	})}, nil
}

// SetDefault selects the connection for later runs of this principal.
func (r *Registry) SetDefault(ctx context.Context, ref Ref) error {
	if ref.ModelID == "" {
		return ErrNotConfigured
	}
	res, e := r.db.ExecContext(ctx, `INSERT OR REPLACE INTO model_defaults SELECT app,user,id,? FROM model_connections WHERE app=? AND user=? AND id=? AND disabled=0`, ref.ModelID, r.scope.App, r.scope.User, ref.ConnectionID)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if n == 0 {
		return ErrNotConfigured
	}
	return e
}
func (r *Registry) Default(ctx context.Context, scope Scope) (Ref, error) {
	var ref Ref
	if scope != r.scope {
		return ref, ErrNotConfigured
	}
	e := r.db.QueryRowContext(ctx, `SELECT d.connection,d.model FROM model_defaults d JOIN model_connections c ON c.id=d.connection AND c.app=d.app AND c.user=d.user WHERE d.app=? AND d.user=? AND c.disabled=0`, scope.App, scope.User).Scan(&ref.ConnectionID, &ref.ModelID)
	if errors.Is(e, sql.ErrNoRows) {
		return ref, ErrNotConfigured
	}
	return ref, e
}

// AllowBackground records the user's explicit grant for hosted OAuth tasks.
func (r *Registry) AllowBackground(ctx context.Context, id string, allow bool) error {
	res, e := r.db.ExecContext(ctx, "UPDATE model_connections SET background=? WHERE id=? AND app=? AND user=?", allow, id, r.scope.App, r.scope.User)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if n == 0 {
		return ErrNotConfigured
	}
	return e
}

func httpErrorClass(status int, code string) string {
	if status == 429 && code == "insufficient_quota" {
		return "quota"
	}
	if status == 404 && code == "model_not_found" {
		return "model_not_found"
	}
	switch status {
	case 401:
		return "authentication"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 402:
		return "quota"
	case 429:
		return "rate_limited"
	case 400, 415, 422:
		return "invalid_request"
	}
	if status >= 500 {
		return "upstream"
	}
	return ""
}
