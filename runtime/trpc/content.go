package trpcruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/lookerjin/grape-core/agent"
	"github.com/santhosh-tekuri/jsonschema/v6"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

var fileIDPattern = regexp.MustCompile(`^file-[A-Za-z0-9_-]{1,128}$`)
var schemaName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// The OpenAI adapter infers three variant protocols from their official hosts,
// even for an openai_compatible config. Some variants omit binary content.
func (rt *Runtime) protocol() string {
	provider := rt.cfg.Model.Provider
	if provider != "" && provider != "openai" && provider != "openai_compatible" {
		return provider
	}
	endpoint, err := url.Parse(rt.cfg.Model.BaseURL)
	if err != nil {
		return provider
	}
	switch strings.ToLower(endpoint.Hostname()) {
	case "api.deepseek.com":
		return "deepseek"
	case "api.minimax.io", "api.minimaxi.com":
		return "minimax"
	case "api.moonshot.ai", "api.moonshot.cn":
		return "kimi"
	}
	return provider
}

func (rt *Runtime) inputMessage(req agent.RunRequest) (model.Message, error) {
	msg := model.NewUserMessage(req.Message)
	if len(req.Attachments) == 0 {
		return msg, nil
	}
	if len(req.Attachments) > 16 {
		return msg, fmt.Errorf("at most 16 attachments")
	}
	provider := rt.protocol()
	if !rt.injectedModel && !slices.Contains([]string{"", "openai", "openai_compatible", "anthropic", "gemini"}, provider) {
		return msg, fmt.Errorf("attachment support is not established for provider %q", provider)
	}
	if req.Message != "" {
		msg.Content = ""
		msg.ContentParts = []model.ContentPart{{Type: model.ContentTypeText, Text: &req.Message}}
	}
	total := 0
	for _, a := range req.Attachments {
		mt, params, err := mime.ParseMediaType(a.MIME)
		if err != nil || len(params) > 0 || (!strings.Contains(mt, "/") || strings.Contains(mt, "*")) {
			return msg, fmt.Errorf("invalid attachment MIME")
		}
		if a.Name != "" && !validSegment(a.Name) {
			return msg, fmt.Errorf("invalid display name")
		}
		if a.FileID != "" {
			if a.Kind != "file" || len(a.Data) != 0 || !fileIDPattern.MatchString(a.FileID) || (!rt.injectedModel && provider != "openai" && provider != "" && provider != "openai_compatible") {
				return msg, fmt.Errorf("unsupported provider file reference")
			}
			msg.ContentParts = append(msg.ContentParts, model.ContentPart{Type: model.ContentTypeFile, File: &model.File{Name: a.Name, FileID: a.FileID, MimeType: mt}})
			continue
		}
		if len(a.Data) == 0 || len(a.Data) > 8<<20 || total+len(a.Data) > 16<<20 {
			return msg, fmt.Errorf("empty attachment or attachment size limit exceeded")
		}
		total += len(a.Data)
		switch a.Kind {
		case "image":
			if !slices.Contains([]string{"image/png", "image/jpeg", "image/webp", "image/gif"}, mt) || http.DetectContentType(a.Data) != mt {
				return msg, fmt.Errorf("unsupported or mismatched image MIME")
			}
			if a.Name != "" {
				return msg, fmt.Errorf("native image parts have no filename")
			}
			msg.AddImageData(bytes.Clone(a.Data), "auto", strings.TrimPrefix(mt, "image/"))
		case "file":
			if a.Name == "" {
				return msg, fmt.Errorf("inline file requires name")
			}
			if !rt.injectedModel && provider == "anthropic" && mt != "application/pdf" && mt != "text/plain" {
				return msg, fmt.Errorf("unsupported Anthropic document MIME")
			}
			msg.AddFileData(a.Name, bytes.Clone(a.Data), mt)
		default:
			return msg, fmt.Errorf("unknown attachment kind %q", a.Kind)
		}
	}
	return msg, nil
}

func (rt *Runtime) outputOption(out *agent.OutputSchema) (*jsonschema.Schema, trpcagent.RunOption, error) {
	if out == nil {
		return nil, nil, nil
	}
	if !rt.injectedModel && !slices.Contains([]string{"", "openai", "openai_compatible", "gemini"}, rt.protocol()) {
		return nil, nil, fmt.Errorf("provider does not expose native schema mode")
	}
	var doc map[string]any
	if !schemaName.MatchString(out.Name) || len(out.Schema) > 65536 || json.Unmarshal(out.Schema, &doc) != nil || doc == nil {
		return nil, nil, fmt.Errorf("invalid schema name or JSON object")
	}
	if doc["type"] != "object" && doc["type"] != "array" {
		return nil, nil, fmt.Errorf("native extraction supports object or array roots")
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(jsonschema.SchemeURLLoader{})
	const location = "urn:grape:output"
	if err := c.AddResource(location, doc); err != nil {
		return nil, nil, err
	}
	schema, err := c.Compile(location)
	if err != nil {
		return nil, nil, err
	}
	return schema, trpcagent.WithStructuredOutputJSONSchema(out.Name, doc, out.Strict, out.Description), nil
}

func attachmentPart(p model.ContentPart) (agent.Attachment, bool) {
	if p.Image != nil && len(p.Image.Data) > 0 {
		return agent.Attachment{Kind: "image", MIME: "image/" + strings.TrimPrefix(p.Image.Format, "image/"), Data: p.Image.Data}, true
	}
	if p.File != nil && (len(p.File.Data) > 0 || p.File.FileID != "") {
		return agent.Attachment{Kind: "file", Name: p.File.Name, MIME: p.File.MimeType, FileID: p.File.FileID, Data: p.File.Data}, true
	}
	return agent.Attachment{}, false
}

// ReadAttachment returns a retained user attachment from this app/user/session.
func (rt *Runtime) ReadAttachment(ctx context.Context, user, id string, ref agent.AttachmentRef) (agent.Attachment, error) {
	var empty agent.Attachment
	if user == "" || id == "" || ref.EventID == "" || ref.Choice < 0 || ref.Part < 0 {
		return empty, agent.Invalid("attachment", fmt.Errorf("invalid reference"))
	}
	window, err := rt.sessions.GetEventWindow(ctx, session.EventWindowRequest{Key: session.Key{AppName: rt.appName, UserID: user, SessionID: id}, AnchorEventID: ref.EventID, Roles: []model.Role{model.RoleUser}})
	if err != nil {
		return empty, agent.Wrap(agent.ErrSession, "attachment", err)
	}
	for _, entry := range window.Entries {
		e := entry.Event
		if e.ID != ref.EventID || e.Response == nil || ref.Choice >= len(e.Choices) {
			continue
		}
		m := e.Choices[ref.Choice].Message
		if m.Role == model.RoleUser && ref.Part < len(m.ContentParts) {
			if a, ok := attachmentPart(m.ContentParts[ref.Part]); ok {
				return a, nil
			}
		}
	}
	return empty, agent.Wrap(agent.ErrSession, "attachment", fmt.Errorf("reference not found"))
}
