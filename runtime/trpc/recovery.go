package trpcruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/coding"
)

const initRecovery = `CREATE TABLE IF NOT EXISTS grape_file_changes(
 id INTEGER PRIMARY KEY,state_id INTEGER NOT NULL REFERENCES session_states(id) ON DELETE CASCADE,run TEXT NOT NULL,root TEXT NOT NULL,path TEXT NOT NULL,before BLOB NOT NULL,expected TEXT NOT NULL,status TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS grape_changes_scope ON grape_file_changes(state_id,id);
CREATE TABLE IF NOT EXISTS grape_recoveries(id TEXT PRIMARY KEY,state_id INTEGER NOT NULL REFERENCES session_states(id) ON DELETE CASCADE,run TEXT NOT NULL,kind TEXT NOT NULL,status TEXT NOT NULL,detail TEXT NOT NULL,at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS grape_backups(op TEXT NOT NULL REFERENCES grape_recoveries(id) ON DELETE CASCADE,root TEXT NOT NULL,path TEXT NOT NULL,image BLOB NOT NULL,first_change INTEGER NOT NULL,last_change INTEGER NOT NULL,target TEXT NOT NULL,status TEXT NOT NULL,PRIMARY KEY(op,root,path));
CREATE TRIGGER IF NOT EXISTS grape_forget_deleted AFTER UPDATE OF deleted_at ON session_states WHEN NEW.deleted_at IS NOT NULL BEGIN
DELETE FROM grape_file_changes WHERE state_id=NEW.id;DELETE FROM grape_turns WHERE state_id=NEW.id;DELETE FROM grape_recoveries WHERE state_id=NEW.id;END`

func (rt *Runtime) saveTracked(ctx context.Context, d *coding.Directory, in saveIn) error {
	if rt.turnDB == nil {
		return d.Save(in.FileName, in.Contents, in.Overwrite)
	}
	return rt.trackWrite(ctx, d, in.FileName, func(i coding.FileImage) (coding.FileImage, error) {
		if i.Exists && !in.Overwrite {
			return i, errors.New("file exists")
		}
		if !utf8.ValidString(in.Contents) {
			return i, errors.New("invalid UTF-8")
		}
		if !i.Exists {
			i.Mode = 0644
		}
		i.Exists = true
		i.Data = []byte(in.Contents)
		return i, nil
	})
}
func (rt *Runtime) replaceTracked(ctx context.Context, d *coding.Directory, in replaceIn) error {
	if rt.turnDB == nil {
		return d.Replace(in.FileName, in.OldString, in.NewString, in.NumReplacements)
	}
	return rt.trackWrite(ctx, d, in.FileName, func(i coding.FileImage) (coding.FileImage, error) {
		if !i.Exists || !utf8.Valid(i.Data) || in.OldString == "" || !strings.Contains(string(i.Data), in.OldString) {
			return i, errors.New("no matching UTF-8 content")
		}
		n := in.NumReplacements
		if n <= 0 {
			n = 1
		}
		i.Data = []byte(strings.Replace(string(i.Data), in.OldString, in.NewString, n))
		return i, nil
	})
}

func (rt *Runtime) trackWrite(ctx context.Context, d *coding.Directory, path string, change func(coding.FileImage) (coding.FileImage, error)) error {
	st := stateFrom(ctx)
	if st == nil {
		return errors.New("missing run scope")
	}
	path = filepath.Clean(path)
	before, err := d.Image(path)
	if errors.Is(err, coding.ErrImageTooLarge) {
		return agent.Wrap(agent.ErrCheckpointLimit, "file", err)
	}
	if err != nil {
		return err
	}
	after, err := change(before)
	if err != nil {
		return err
	}
	if len(after.Data) > coding.MaxImageBytes {
		return agent.Wrap(agent.ErrCheckpointLimit, "file", coding.ErrImageTooLarge)
	}
	expected := after.Digest()
	if before.Digest() == expected {
		return nil
	}
	raw, err := json.Marshal(before)
	if err != nil {
		return err
	}
	res, err := rt.turnDB.ExecContext(ctx, `INSERT INTO grape_file_changes(state_id,run,root,path,before,expected,status)
	SELECT id,?,?,?,?,?,'pending' FROM session_states s WHERE `+sessionScope+` AND COALESCE((SELECT SUM(length(before)) FROM grape_file_changes WHERE state_id=s.id),0)+?<=33554432`, st.runID, d.Path(), path, raw, expected, rt.appName, st.user, st.session, len(raw))
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return agent.Wrap(agent.ErrCheckpointLimit, "session changes", errors.New("checkpoint could not be reserved"))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	err = d.ReplaceImage(path, before.Digest(), after)
	status := "applied"
	if err != nil {
		status = "failed"
	}
	_, e := rt.turnDB.ExecContext(context.WithoutCancel(ctx), "UPDATE grape_file_changes SET status=? WHERE id=?", status, id)
	return errors.Join(err, e)
}

type restoreFile struct {
	agent.FileRestoreItem
	image       coding.FileImage
	first, last int64
}

func (rt *Runtime) restorePlan(ctx context.Context, user, id, run string) (agent.FileRestorePlan, []restoreFile, error) {
	plan := agent.FileRestorePlan{TrackingScope: []string{"save_file", "replace_content"}}
	if err := rt.checkpointScope(user, id); err != nil {
		return plan, nil, err
	}
	if run == "" {
		return plan, nil, agent.Invalid("checkpoint", errors.New("missing run"))
	}
	tx, e := rt.turnDB.BeginTx(ctx, nil)
	if e != nil {
		return plan, nil, e
	}
	defer tx.Rollback()
	b, e := rt.boundary(ctx, tx, user, id, run)
	if e != nil {
		return plan, nil, e
	}
	rows, e := tx.QueryContext(ctx, "SELECT c.id,root,path,before,expected,status FROM grape_file_changes c JOIN session_states s ON s.id=c.state_id WHERE s."+sessionScope+" AND c.id>? AND status IN ('applied','pending') ORDER BY root,path,c.id", rt.appName, user, id, b.fileID)
	if e != nil {
		return plan, nil, e
	}
	var files []restoreFile
	for rows.Next() {
		var seq int64
		var root, path, expected, state string
		var raw []byte
		if e = rows.Scan(&seq, &root, &path, &raw, &expected, &state); e != nil {
			rows.Close()
			return plan, nil, e
		}
		var img coding.FileImage
		if e = json.Unmarshal(raw, &img); e != nil {
			rows.Close()
			return plan, nil, e
		}
		idx := len(files) - 1
		if idx < 0 || files[idx].Root != root || files[idx].Path != path {
			files = append(files, restoreFile{FileRestoreItem: agent.FileRestoreItem{Root: root, Path: path, Target: img.Digest()}, image: img, first: seq})
			idx++
		} else if files[idx].Expected != img.Digest() {
			files[idx].Conflict = "external edit between recorded writes"
		}
		files[idx].last = seq
		files[idx].Expected = expected
		if state == "pending" {
			files[idx].Conflict = "interrupted write with unknown outcome"
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return plan, nil, e
	}
	if e = tx.Commit(); e != nil {
		return plan, nil, e
	}
	hash := sha256.New()
	enc := json.NewEncoder(hash)
	_ = enc.Encode([]string{rt.appName, user, id, run})
	for i := range files {
		f := &files[i]
		switch {
		case f.Target == "absent":
			f.Action = "delete"
		case f.Expected == "absent":
			f.Action = "create"
		default:
			f.Action = "replace"
		}
		if !rt.recoveryRootAllowed(f.Root) {
			f.Conflict = "workspace root no longer granted"
			f.Actual = "unknown"
		} else {
			d, e := coding.OpenDirectory(f.Root)
			if e == nil {
				var actual coding.FileImage
				actual, e = d.Image(f.Path)
				d.Close()
				if e == nil {
					f.Actual = actual.Digest()
				}
			}
			if e != nil {
				f.Conflict = "unreadable path"
			} else if f.Actual != f.Expected && f.Conflict == "" {
				var prepared bool
				if f.Actual == f.Target {
					e = rt.turnDB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM grape_backups b JOIN grape_recoveries r ON r.id=b.op JOIN session_states s ON s.id=r.state_id WHERE s."+sessionScope+" AND r.run=? AND b.root=? AND b.path=? AND b.first_change=? AND b.last_change=? AND b.target=? AND b.status='prepared')", rt.appName, user, id, run, f.Root, f.Path, f.first, f.last, f.Target).Scan(&prepared)
					if e != nil {
						return plan, nil, e
					}
				}
				if prepared {
					f.Action = "confirm_previous_restore"
				} else {
					f.Conflict = "current file differs from recorded write"
				}
			}
		}
		plan.Files = append(plan.Files, f.FileRestoreItem)
		_ = enc.Encode(f.FileRestoreItem)
		_ = enc.Encode([]int64{f.first, f.last})
	}
	plan.Digest = fmt.Sprintf("%x", hash.Sum(nil))
	return plan, files, nil
}

// PreviewFiles reads the current workspace and makes no changes.
func (rt *Runtime) PreviewFiles(ctx context.Context, user, id, run string) (agent.FileRestorePlan, error) {
	p, _, e := rt.restorePlan(ctx, user, id, run)
	return p, e
}

func (rt *Runtime) beginRecovery(ctx context.Context, user, id, run, kind string) (agent.RecoveryRecord, error) {
	r := agent.RecoveryRecord{ID: uuid.NewString(), RunID: run, Kind: kind, Status: "pending", CreatedAt: time.Now().UTC()}
	res, e := rt.turnDB.ExecContext(ctx, `INSERT INTO grape_recoveries SELECT ?,id,?,?,?,'',? FROM session_states WHERE `+sessionScope, r.ID, run, kind, r.Status, r.CreatedAt.UnixNano(), rt.appName, user, id)
	if e != nil {
		return r, e
	}
	n, e := res.RowsAffected()
	if n != 1 {
		return r, agent.ErrSession
	}
	return r, e
}

// RestoreFiles applies a confirmed preview. It rechecks every path and saves
// the current image durably before changing the workspace.
func (rt *Runtime) RestoreFiles(ctx context.Context, user, id, run, digest string) (r agent.RecoveryRecord, err error) {
	if e := rt.checkpointScope(user, id); e != nil {
		return r, e
	}
	if rt.cfg.Workspace.Permission == agent.PermissionReadOnly {
		return r, agent.ErrPermissionDenied
	}
	if digest == "" {
		return r, agent.Invalid("confirmation", errors.New("preview digest required"))
	}
	unlock, e := rt.lockSession(ctx, user, id)
	if e != nil {
		return r, e
	}
	defer unlock()
	plan, files, e := rt.restorePlan(ctx, user, id, run)
	if e != nil {
		return r, e
	}
	r, e = rt.beginRecovery(ctx, user, id, run, "files")
	if e != nil {
		return r, e
	}
	count := 0
	defer func() {
		r.Status = "completed"
		if err != nil {
			r.Status = "refused"
			if count > 0 {
				r.Status = "partial"
			}
			r.Detail = err.Error()
		}
		_, e := rt.turnDB.ExecContext(context.WithoutCancel(ctx), "UPDATE grape_recoveries SET status=?,detail=? WHERE id=?", r.Status, r.Detail, r.ID)
		if e != nil {
			r.Status = "unknown"
			err = errors.Join(err, e)
		}
	}()
	if plan.Digest != digest {
		return r, errors.New("preview changed; review again")
	}
	for _, f := range files {
		if f.Conflict != "" {
			return r, fmt.Errorf("%s: %s", f.Path, f.Conflict)
		}
	}
	for _, f := range files {
		if err = ctx.Err(); err != nil {
			return r, err
		}
		d, e := coding.OpenDirectory(f.Root)
		if e != nil {
			return r, e
		}
		e = func() error {
			defer d.Close()
			actual, e := d.Image(f.Path)
			if e != nil {
				return e
			}
			if f.Action == "confirm_previous_restore" {
				if actual.Digest() != f.Target {
					return fmt.Errorf("%s changed since preview", f.Path)
				}
				count++
				return rt.markRestored(ctx, user, id, run, f)
			}
			if actual.Digest() != f.Expected {
				return fmt.Errorf("%s changed since preview", f.Path)
			}
			raw, e := json.Marshal(actual)
			if e != nil {
				return e
			}
			res, e := rt.turnDB.ExecContext(ctx, `INSERT INTO grape_backups SELECT ?,?,?,?,?,?,?,'prepared' WHERE COALESCE((SELECT SUM(length(b.image)) FROM grape_backups b JOIN grape_recoveries r1 ON r1.id=b.op JOIN grape_recoveries r2 ON r1.state_id=r2.state_id WHERE r2.id=?),0)+?<=67108864`, r.ID, f.Root, f.Path, raw, f.first, f.last, f.Target, r.ID, len(raw))
			if e == nil {
				if n, _ := res.RowsAffected(); n != 1 {
					return agent.Wrap(agent.ErrCheckpointLimit, "backups", errors.New("backup could not be reserved"))
				}
			}
			if e != nil {
				return e
			}
			if e = d.ReplaceImage(f.Path, f.Expected, f.image); e != nil {
				return e
			}
			count++
			return rt.markRestored(context.WithoutCancel(ctx), user, id, run, f)
		}()
		if e != nil {
			return r, e
		}
	}
	return r, nil
}

// Recoveries reports context and file results independently. Pending after a
// process interruption means inspect the files and backups before retrying.
func (rt *Runtime) Recoveries(ctx context.Context, user, id string) ([]agent.RecoveryRecord, error) {
	if e := rt.checkpointScope(user, id); e != nil {
		return nil, e
	}
	rows, e := rt.turnDB.QueryContext(ctx, "SELECT r.id,r.run,r.kind,r.status,r.detail,r.at FROM grape_recoveries r JOIN session_states s ON s.id=r.state_id WHERE s."+sessionScope+" ORDER BY r.at", rt.appName, user, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []agent.RecoveryRecord
	for rows.Next() {
		var r agent.RecoveryRecord
		var at int64
		if e = rows.Scan(&r.ID, &r.RunID, &r.Kind, &r.Status, &r.Detail, &at); e != nil {
			return nil, e
		}
		r.CreatedAt = time.Unix(0, at).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecoveryBackup returns the pre-restore image for an exact session, operation and path.
func (rt *Runtime) RecoveryBackup(ctx context.Context, user, id, op, root, path string) (coding.FileImage, error) {
	var image coding.FileImage
	if e := rt.checkpointScope(user, id); e != nil {
		return image, e
	}
	var raw []byte
	e := rt.turnDB.QueryRowContext(ctx, "SELECT b.image FROM grape_backups b JOIN grape_recoveries r ON r.id=b.op JOIN session_states s ON s.id=r.state_id WHERE s."+sessionScope+" AND b.op=? AND b.root=? AND b.path=?", rt.appName, user, id, op, root, path).Scan(&raw)
	if e != nil {
		return image, e
	}
	e = json.Unmarshal(raw, &image)
	return image, e
}

func (rt *Runtime) recoveryRootAllowed(path string) bool {
	if root, e := coding.Resolve(rt.cfg.Workspace.Root); e == nil && root == path {
		return true
	}
	for _, d := range rt.extraRoots {
		if d.Path() == path {
			return true
		}
	}
	return false
}

func (rt *Runtime) markRestored(ctx context.Context, user, id, run string, f restoreFile) error {
	tx, e := rt.turnDB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(ctx, "UPDATE grape_file_changes SET status='restored' WHERE id BETWEEN ? AND ? AND root=? AND path=? AND state_id=(SELECT id FROM session_states WHERE "+sessionScope+")", f.first, f.last, f.Root, f.Path, rt.appName, user, id)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "UPDATE grape_backups SET status='restored' WHERE op IN (SELECT r.id FROM grape_recoveries r JOIN session_states s ON s.id=r.state_id WHERE s."+sessionScope+" AND r.run=?) AND root=? AND path=? AND first_change=? AND last_change=? AND target=?", rt.appName, user, id, run, f.Root, f.Path, f.first, f.last, f.Target)
	if e != nil {
		return e
	}
	return tx.Commit()
}

// PreviewFile returns the current and proposed file images for a product diff.
func (rt *Runtime) PreviewFile(ctx context.Context, user, id, run, root, path string) (current, target coding.FileImage, err error) {
	_, files, e := rt.restorePlan(ctx, user, id, run)
	if e != nil {
		return current, target, e
	}
	for _, f := range files {
		if f.Root == root && f.Path == path && rt.recoveryRootAllowed(root) {
			d, e := coding.OpenDirectory(root)
			if e != nil {
				return current, target, e
			}
			defer d.Close()
			current, e = d.Image(path)
			return current, f.image, e
		}
	}
	return current, target, agent.ErrPermissionDenied
}

// RecoveryBackups enumerates paths even after a crash interrupted RestoreFiles.
func (rt *Runtime) RecoveryBackups(ctx context.Context, user, id, op string) ([]agent.BackupInfo, error) {
	if e := rt.checkpointScope(user, id); e != nil {
		return nil, e
	}
	rows, e := rt.turnDB.QueryContext(ctx, "SELECT b.root,b.path,b.status FROM grape_backups b JOIN grape_recoveries r ON r.id=b.op JOIN session_states s ON s.id=r.state_id WHERE s."+sessionScope+" AND b.op=? ORDER BY b.root,b.path", rt.appName, user, id, op)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []agent.BackupInfo
	for rows.Next() {
		var b agent.BackupInfo
		if e = rows.Scan(&b.Root, &b.Path, &b.Status); e != nil {
			return nil, e
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RecoveryLimits reports the current fixed limits without scanning the workspace.
func (rt *Runtime) RecoveryLimits() agent.RecoveryLimits {
	return agent.RecoveryLimits{FileBytes: coding.MaxImageBytes, SessionChangeBytes: 32 << 20, SessionBackupBytes: 64 << 20}
}
