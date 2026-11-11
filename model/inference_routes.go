package model

import (
	"encoding/json"
	"errors"
	"path"
	"strings"
)

// InferenceRoutes binds an OAuth registration to relative paths on its own
// inference origin. DecodeModels receives only the bounded catalog body.
type InferenceRoutes struct {
	Generate, Models string
	DecodeModels     func(json.RawMessage) ([]string, error)
}

func (p OAuthProvider) WithInferenceRoutes(routes InferenceRoutes) OAuthProvider {
	p.routes = routes
	return p
}
func (p OAuthProvider) inferenceRoutes() InferenceRoutes {
	w := p.routes
	if w.Generate == "" {
		w.Generate = p.nativeGeneratePath()
	}
	if w.Models == "" {
		w.Models = "/models"
	}
	return w
}
func (p OAuthProvider) nativeGeneratePath() string {
	if p.provider.Protocol == Messages {
		return "/v1/messages"
	}
	return "/chat/completions"
}
func validRoute(s string) bool {
	if !strings.HasPrefix(s, "/") || s == "/" || path.Clean(s) != s {
		return false
	}
	for _, c := range s {
		if c != '/' && c != '-' && c != '_' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func defaultModelIDs(data json.RawMessage) ([]string, error) {
	var body struct{ Data []struct{ ID string } }
	if json.Unmarshal(data, &body) != nil || body.Data == nil {
		return nil, errors.New("invalid models list")
	}
	out := make([]string, 0, len(body.Data))
	for _, v := range body.Data {
		out = append(out, v.ID)
	}
	return out, nil
}
