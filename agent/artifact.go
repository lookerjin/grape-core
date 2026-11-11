package agent

import "context"

// Artifact is a pinned revision. Size is measured in bytes.
type Artifact struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	MIME    string `json:"mime"`
	Size    int    `json:"size"`
}

// ArtifactStore is bound to one app, user and session.
type ArtifactStore interface {
	Save(context.Context, string, string, []byte) (Artifact, error)
	Read(context.Context, string, *int) (Artifact, []byte, error)
	List(context.Context) ([]string, error)
	Versions(context.Context, string) ([]int, error)
	Delete(context.Context, string) error
}
