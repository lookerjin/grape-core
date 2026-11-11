package trpcruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lookerjin/grape-core/agent"
	"trpc.group/trpc-go/trpc-agent-go/session"
	sqlitesession "trpc.group/trpc-go/trpc-agent-go/session/sqlite"
)

// The v1.12.0 SQLite service does not expose an atomic prefix copy or truncate.
// Keep boundaries in its database; event bodies stay in its native tables.
const initTurns = `CREATE TABLE IF NOT EXISTS grape_turns (
	state_id INTEGER NOT NULL REFERENCES session_states(id) ON DELETE CASCADE,
	run_id TEXT NOT NULL, last_event INTEGER NOT NULL, last_track INTEGER NOT NULL,
	last_file INTEGER NOT NULL DEFAULT 0, state BLOB NOT NULL, parent_session TEXT NOT NULL DEFAULT '', parent_run TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(state_id,run_id))`

const sessionScope = `app_name=? AND user_id=? AND session_id=? AND deleted_at IS NULL`
const turnSelect = `SELECT t.run_id,t.last_event,t.last_track,t.last_file,t.state,t.parent_session,t.parent_run
	FROM grape_turns t JOIN session_states s ON s.id=t.state_id WHERE s.` + sessionScope

func (rt *Runtime) checkpointScope(user, id string) error {
	if rt.turnDB == nil {
		return agent.Invalid("checkpoints", errors.New("enable TurnCheckpoints at Runtime creation"))
	}
	if id == "" || user == "" {
		return agent.Invalid("session", errors.New("missing principal or session"))
	}
	if rt.registry != nil && rt.registry.Scope().User != user {
		return agent.ErrPermissionDenied
	}
	return nil
}

// ensureBaseline is called under the session lock after request validation,
// before Runner.Run can append input or execute a tool.
func (rt *Runtime) ensureBaseline(ctx context.Context, user, id string) error {
	key := session.Key{AppName: rt.appName, UserID: user, SessionID: id}
	sess, err := rt.sessions.GetSession(ctx, key, session.WithEventNum(1))
	if err != nil {
		return err
	}
	if sess == nil {
		if _, err = rt.sessions.CreateSession(ctx, key, nil); err != nil {
			return err
		}
	}
	// Never invent an original boundary for pre-existing conversation or writes.
	// A baseline at event/file zero is meaningful only for a genuinely empty session.
	_, err = rt.turnDB.ExecContext(ctx, `INSERT OR IGNORE INTO grape_turns(state_id,run_id,last_event,last_track,last_file,state)
	SELECT s.id,?,0,0,0,s.state FROM session_states s WHERE `+sessionScope+`
	AND NOT EXISTS(SELECT 1 FROM session_events WHERE `+sessionScope+`)
	AND NOT EXISTS(SELECT 1 FROM session_track_events WHERE `+sessionScope+`)
	AND NOT EXISTS(SELECT 1 FROM grape_file_changes WHERE state_id=s.id)
	AND NOT EXISTS(SELECT 1 FROM grape_turns WHERE state_id=s.id)`,
		agent.SessionBaselineID, rt.appName, user, id, rt.appName, user, id, rt.appName, user, id)
	return err
}

func (rt *Runtime) commitTurn(ctx context.Context, user, id, run string) error {
	res, err := rt.turnDB.ExecContext(ctx, `INSERT INTO grape_turns(state_id,run_id,last_event,last_track,last_file,state)
	SELECT s.id,?,(SELECT MAX(id) FROM session_events WHERE `+sessionScope+` AND json_extract(CAST(event AS TEXT),'$.requestID')=?),
	COALESCE((SELECT MAX(id) FROM session_track_events WHERE `+sessionScope+`),0),COALESCE((SELECT MAX(id) FROM grape_file_changes WHERE state_id=s.id),0),s.state FROM session_states s WHERE `+sessionScope,
		run, rt.appName, user, id, run, rt.appName, user, id, rt.appName, user, id)
	if err != nil {
		return err
	}
	n, e := res.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return e
}

type boundary struct {
	agent.TurnCheckpoint
	eventID, trackID, fileID int64
	state                    []byte
}

func (rt *Runtime) boundary(ctx context.Context, tx *sql.Tx, user, id, run string) (boundary, error) {
	var b boundary
	q := turnSelect
	args := []any{rt.appName, user, id}
	if run != "" {
		q += " AND t.run_id=?"
		args = append(args, run)
	}
	q += " ORDER BY t.last_event DESC LIMIT 1"
	err := tx.QueryRowContext(ctx, q, args...).Scan(&b.RunID, &b.eventID, &b.trackID, &b.fileID, &b.state, &b.ParentSession, &b.ParentRun)
	if err != nil {
		return b, agent.Wrap(agent.ErrSession, "checkpoint", err)
	}
	b.Baseline = b.RunID == agent.SessionBaselineID
	return b, nil
}

// Checkpoints lists the original baseline, when known, followed by successful turn boundaries.
func (rt *Runtime) Checkpoints(ctx context.Context, user, id string) ([]agent.TurnCheckpoint, error) {
	if err := rt.checkpointScope(user, id); err != nil {
		return nil, err
	}
	rows, err := rt.turnDB.QueryContext(ctx, `SELECT t.run_id,t.parent_session,t.parent_run FROM grape_turns t JOIN session_states s ON s.id=t.state_id WHERE s.`+sessionScope+` ORDER BY t.last_event`, rt.appName, user, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agent.TurnCheckpoint
	for rows.Next() {
		var b agent.TurnCheckpoint
		if err = rows.Scan(&b.RunID, &b.ParentSession, &b.ParentRun); err != nil {
			return nil, err
		}
		b.Baseline = b.RunID == agent.SessionBaselineID
		out = append(out, b)
	}
	return out, rows.Err()
}

// ForkSession creates a new session from a baseline or completed turn in one SQLite transaction.
func (rt *Runtime) ForkSession(ctx context.Context, user, id, run string) (string, error) {
	if err := rt.checkpointScope(user, id); err != nil {
		return "", err
	}
	if run == "" {
		return "", agent.Invalid("checkpoint", errors.New("run ID required"))
	}
	tx, err := rt.turnDB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	b, err := rt.boundary(ctx, tx, user, id, run)
	if err != nil {
		return "", err
	}
	dest := uuid.NewString()
	now := time.Now().UTC()
	var state sqlitesession.SessionState
	if err = json.Unmarshal(b.state, &state); err != nil {
		return "", err
	}
	state.ID = dest
	// Native event windows filter on the session creation epoch. Preserve
	// that epoch together with the original event timestamps and IDs.
	state.UpdatedAt = now
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO session_states(app_name,user_id,session_id,state,created_at,updated_at) VALUES(?,?,?,?,?,?)`, rt.appName, user, dest, raw, state.CreatedAt.UnixNano(), now.UnixNano())
	if err != nil {
		return "", err
	}
	sid, err := res.LastInsertId()
	if err != nil {
		return "", err
	}
	var last [2]int64
	for i, part := range []struct {
		table, cols string
		end         int64
	}{{"session_events", "event", b.eventID}, {"session_track_events", "track,event", b.trackID}} {
		if part.end == 0 {
			continue
		}
		cols := part.cols + ",created_at,updated_at,expires_at"
		q := fmt.Sprintf("INSERT INTO %s(app_name,user_id,session_id,%s) SELECT app_name,user_id,?,%s FROM %s WHERE %s AND id<=? ORDER BY id", part.table, cols, cols, part.table, sessionScope)
		res, err = tx.ExecContext(ctx, q, dest, rt.appName, user, id, part.end)
		if err != nil {
			return "", err
		}
		last[i], err = res.LastInsertId()
		if err != nil {
			return "", err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO grape_turns(state_id,run_id,last_event,last_track,state,parent_session,parent_run) VALUES(?,?,?,?,?,?,?)`, sid, run, last[0], last[1], raw, id, run)
	if err != nil {
		return "", err
	}
	return dest, tx.Commit()
}

// RewindSession atomically restores conversation state. It does not edit workspace files.
func (rt *Runtime) RewindSession(ctx context.Context, user, id, run string) (err error) {
	if err := rt.checkpointScope(user, id); err != nil {
		return err
	}
	if run == "" {
		return agent.Invalid("checkpoint", errors.New("run ID required"))
	}
	unlock, err := rt.lockSession(ctx, user, id)
	if err != nil {
		return err
	}
	defer unlock()
	record, e := rt.beginRecovery(ctx, user, id, run, "conversation")
	if e != nil {
		return e
	}
	defer func() {
		if err != nil {
			_, e := rt.turnDB.ExecContext(context.WithoutCancel(ctx), "UPDATE grape_recoveries SET status='failed',detail=? WHERE id=?", err.Error(), record.ID)
			err = errors.Join(err, e)
		}
	}()
	tx, err := rt.turnDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, err := rt.boundary(ctx, tx, user, id, run)
	if err != nil {
		return err
	}
	for _, step := range []struct {
		q    string
		args []any
	}{
		{"DELETE FROM session_events WHERE " + sessionScope + " AND id>?", []any{rt.appName, user, id, b.eventID}},
		{"DELETE FROM session_track_events WHERE " + sessionScope + " AND id>?", []any{rt.appName, user, id, b.trackID}},
		{"DELETE FROM session_summaries WHERE " + sessionScope, []any{rt.appName, user, id}},
		{"DELETE FROM grape_turns WHERE state_id=(SELECT id FROM session_states WHERE " + sessionScope + ") AND last_event>?", []any{rt.appName, user, id, b.eventID}},
		{"UPDATE session_states SET state=?,updated_at=? WHERE " + sessionScope, []any{b.state, time.Now().UTC().UnixNano(), rt.appName, user, id}},
	} {
		if _, err = tx.ExecContext(ctx, step.q, step.args...); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE grape_recoveries SET status='completed' WHERE id=?", record.ID); err != nil {
		return err
	}
	return tx.Commit()
}
