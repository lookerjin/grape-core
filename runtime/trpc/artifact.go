package trpcruntime

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lookerjin/grape-core/agent"
	native "trpc.group/trpc-go/trpc-agent-go/artifact"
)

func validSegment(s string) bool {
	return s != "" && len(s) <= 255 && utf8.ValidString(s) && s != "." && s != ".." && !strings.ContainsAny(s, "/\\:") && strings.IndexFunc(s, unicode.IsControl) < 0
}

type artifactScopeKey struct{}
type scopedArtifacts struct {
	rt    *Runtime
	scope native.SessionInfo
}

// Artifacts opens the artifact namespace of the specified principal and session.
// The product authenticates the principal before selecting this scope.
func (rt *Runtime) Artifacts(user, session string) (agent.ArtifactStore, error) {
	if rt.artifacts == nil {
		return nil, fmt.Errorf("artifact service disabled")
	}
	scope := native.SessionInfo{AppName: rt.appName, UserID: user, SessionID: session}
	if !validSegment(scope.AppName) || !validSegment(user) || !validSegment(session) || session == "user" {
		return nil, fmt.Errorf("invalid artifact scope")
	}
	return &scopedArtifacts{rt, scope}, nil
}

// ArtifactsFromContext exposes only the current run's scope to callable tools.
func ArtifactsFromContext(ctx context.Context) (agent.ArtifactStore, error) {
	s, ok := ctx.Value(artifactScopeKey{}).(*scopedArtifacts)
	if !ok {
		return nil, fmt.Errorf("no active artifact scope")
	}
	return s, nil
}
func (s *scopedArtifacts) begin(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validSegment(name) {
		return fmt.Errorf("invalid artifact name")
	}
	if other, ok := ctx.Value(artifactScopeKey{}).(*scopedArtifacts); ok && (other.rt != s.rt || other.scope != s.scope) {
		return fmt.Errorf("artifact scope mismatch")
	}
	s.rt.mu.Lock()
	defer s.rt.mu.Unlock()
	if s.rt.closed {
		return fmt.Errorf("runtime closed")
	}
	s.rt.starting.Add(1)
	return nil
}
func (s *scopedArtifacts) Save(ctx context.Context, name, mt string, data []byte) (agent.Artifact, error) {
	if err := s.begin(ctx, name); err != nil {
		return agent.Artifact{}, err
	}
	defer s.rt.starting.Done()
	if _, _, err := mime.ParseMediaType(mt); err != nil {
		return agent.Artifact{}, err
	}
	v, err := s.rt.artifacts.SaveArtifact(ctx, s.scope, name, &native.Artifact{Data: bytes.Clone(data), Name: name, MimeType: mt})
	if err != nil {
		return agent.Artifact{}, err
	}
	meta := agent.Artifact{Name: name, MIME: mt, Version: v, Size: len(data)}
	if st := stateFrom(ctx); st != nil {
		st.add(agent.Event{RunID: st.runID, SessionID: s.scope.SessionID, Kind: agent.EventArtifact, Payload: agent.MustPayload(meta)})
	}
	return meta, nil
}
func (s *scopedArtifacts) Read(ctx context.Context, name string, version *int) (agent.Artifact, []byte, error) {
	if err := s.begin(ctx, name); err != nil {
		return agent.Artifact{}, nil, err
	}
	defer s.rt.starting.Done()
	v := 0
	if version == nil {
		vs, err := s.rt.artifacts.ListVersions(ctx, s.scope, name)
		if err != nil {
			return agent.Artifact{}, nil, err
		}
		if len(vs) == 0 {
			return agent.Artifact{}, nil, fmt.Errorf("artifact not found")
		}
		v = slices.Max(vs)
	} else {
		v = *version
	}
	if v < 0 {
		return agent.Artifact{}, nil, fmt.Errorf("negative artifact version")
	}
	a, err := s.rt.artifacts.LoadArtifact(ctx, s.scope, name, &v)
	if err != nil {
		return agent.Artifact{}, nil, err
	}
	if a == nil {
		return agent.Artifact{}, nil, fmt.Errorf("artifact not found")
	}
	return agent.Artifact{Name: name, MIME: a.MimeType, Version: v, Size: len(a.Data)}, bytes.Clone(a.Data), nil
}
func (s *scopedArtifacts) List(ctx context.Context) ([]string, error) {
	if err := s.begin(ctx, "list"); err != nil {
		return nil, err
	}
	defer s.rt.starting.Done()
	names, err := s.rt.artifacts.ListArtifactKeys(ctx, s.scope)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		if !validSegment(name) {
			continue
		}
		// Native backends merge their user namespace into this listing. Only
		// expose keys with a revision in the requested session.
		versions, err := s.rt.artifacts.ListVersions(ctx, s.scope, name)
		if err != nil {
			return nil, err
		}
		if len(versions) > 0 {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}
func (s *scopedArtifacts) Versions(ctx context.Context, name string) ([]int, error) {
	if err := s.begin(ctx, name); err != nil {
		return nil, err
	}
	defer s.rt.starting.Done()
	return s.rt.artifacts.ListVersions(ctx, s.scope, name)
}
func (s *scopedArtifacts) Delete(ctx context.Context, name string) error {
	if err := s.begin(ctx, name); err != nil {
		return err
	}
	defer s.rt.starting.Done()
	return s.rt.artifacts.DeleteArtifact(ctx, s.scope, name)
}

// runnerArtifacts enforces the same namespace on native invocation callbacks.
type runnerArtifacts struct{}

func (runnerArtifacts) scope(ctx context.Context, info native.SessionInfo) (*scopedArtifacts, error) {
	s, ok := ctx.Value(artifactScopeKey{}).(*scopedArtifacts)
	if !ok || s.scope != info {
		return nil, fmt.Errorf("artifact scope mismatch")
	}
	return s, nil
}
func (r runnerArtifacts) SaveArtifact(ctx context.Context, i native.SessionInfo, n string, a *native.Artifact) (int, error) {
	s, e := r.scope(ctx, i)
	if e != nil {
		return 0, e
	}
	if a == nil {
		return 0, fmt.Errorf("nil artifact")
	}
	v, e := s.Save(ctx, n, a.MimeType, a.Data)
	return v.Version, e
}
func (r runnerArtifacts) LoadArtifact(ctx context.Context, i native.SessionInfo, n string, v *int) (*native.Artifact, error) {
	s, e := r.scope(ctx, i)
	if e != nil {
		return nil, e
	}
	if e = s.begin(ctx, n); e != nil {
		return nil, e
	}
	defer s.rt.starting.Done()
	if v != nil && *v < 0 {
		return nil, fmt.Errorf("negative artifact version")
	}
	a, e := s.rt.artifacts.LoadArtifact(ctx, i, n, v)
	if e != nil || a == nil {
		return nil, e
	}
	copy := *a
	copy.Data = bytes.Clone(a.Data)
	return &copy, nil
}
func (r runnerArtifacts) ListArtifactKeys(ctx context.Context, i native.SessionInfo) ([]string, error) {
	s, e := r.scope(ctx, i)
	if e != nil {
		return nil, e
	}
	return s.List(ctx)
}
func (r runnerArtifacts) ListVersions(ctx context.Context, i native.SessionInfo, n string) ([]int, error) {
	s, e := r.scope(ctx, i)
	if e != nil {
		return nil, e
	}
	return s.Versions(ctx, n)
}
func (r runnerArtifacts) DeleteArtifact(ctx context.Context, i native.SessionInfo, n string) error {
	s, e := r.scope(ctx, i)
	if e != nil {
		return e
	}
	return s.Delete(ctx, n)
}
