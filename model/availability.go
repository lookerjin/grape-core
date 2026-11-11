package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/lookerjin/grape-core/internal/modelaccess"
)

// AccountModel is local to one connection. Discovery alone leaves availability unknown.
type AccountModel struct {
	ID, Availability, Source string
	ObservedAt               time.Time
}

// Models lists last observed model selections without contacting the provider.
func (r *Registry) Models(ctx context.Context, id string) ([]AccountModel, error) {
	_, _, e := r.connection(ctx, r.db, id)
	if e != nil {
		return nil, e
	}
	rows, e := r.db.QueryContext(ctx, "SELECT model,status,observed FROM model_observations WHERE connection=? ORDER BY model", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []AccountModel
	for rows.Next() {
		var m AccountModel
		var ts string
		if e = rows.Scan(&m.ID, &m.Availability, &ts); e != nil {
			return nil, e
		}
		m.Source = "inference"
		m.ObservedAt, e = time.Parse(time.RFC3339Nano, ts)
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecordResult records a completed native model invocation or a model-specific rejection.
func (r *Registry) RecordResult(ctx context.Context, _ modelaccess.Native, ref Ref, available bool) error {
	status := "unavailable"
	if available {
		status = "available"
	}
	_, e := r.db.ExecContext(ctx, `INSERT OR REPLACE INTO model_observations SELECT id,?,?,? FROM model_connections WHERE id=? AND app=? AND user=?`, ref.ModelID, time.Now().UTC().Format(time.RFC3339Nano), status, ref.ConnectionID, r.scope.App, r.scope.User)
	return e
}

// Discover queries the connection's Chat Completions models endpoint, then merges local observations.
// A provider may advertise models this account cannot invoke.
func (r *Registry) Discover(ctx context.Context, id string) ([]AccountModel, error) {
	b, e := r.resolve(ctx, r.scope, Ref{ConnectionID: id}, false)
	if e != nil {
		return nil, e
	}
	if b.Provider.Protocol != Chat {
		return nil, errors.New("models endpoint not supported for this protocol")
	}
	q, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(b.Provider.Endpoint, "/")+"/models", nil)
	if e != nil {
		return nil, e
	}
	client := b.Client(modelaccess.Native{})
	client.Timeout = 15 * time.Second
	res, e := client.Do(q)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("models endpoint: HTTP %d", res.StatusCode)
	}
	const limit = 8 << 20
	data, e := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if e != nil || len(data) > limit {
		return nil, errors.New("invalid model discovery response")
	}
	var body struct{ Data []struct{ ID string } }
	if json.Unmarshal(data, &body) != nil || body.Data == nil {
		return nil, errors.New("invalid models list")
	}
	known, e := r.Models(ctx, id)
	if e != nil {
		return nil, e
	}
	entries := map[string]AccountModel{}
	for _, v := range known {
		entries[v.ID] = v
	}
	for _, v := range body.Data {
		if v.ID != "" && entries[v.ID].ID == "" {
			entries[v.ID] = AccountModel{ID: v.ID, Availability: "unknown", Source: "provider"}
		}
	}
	out := make([]AccountModel, 0, len(entries))
	for _, v := range entries {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b AccountModel) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}
