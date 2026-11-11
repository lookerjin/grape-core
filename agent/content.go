package agent

import "encoding/json"

// Attachment is either bounded inline data or an already uploaded provider file ID.
type Attachment struct {
	Kind         string `json:"kind"`
	Name         string `json:"name,omitempty"`
	MIME         string `json:"mime"`
	Data         []byte `json:"data,omitempty"`
	ConnectionID string `json:"connection_id,omitempty"`
	FileID       string `json:"file_id,omitempty"`
}

// AttachmentRef locates a part in its original scoped session event.
type AttachmentRef struct {
	EventID string `json:"event_id"`
	Choice  int    `json:"choice"`
	Part    int    `json:"part"`
}
type AttachmentInfo struct {
	Ref          AttachmentRef `json:"ref"`
	Kind         string        `json:"kind"`
	Name         string        `json:"name,omitempty"`
	MIME         string        `json:"mime"`
	Size         int           `json:"size,omitempty"`
	ConnectionID string        `json:"connection_id,omitempty"`
	FileID       string        `json:"file_id,omitempty"`
}
type OutputSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Strict      bool            `json:"strict,omitempty"`
}
