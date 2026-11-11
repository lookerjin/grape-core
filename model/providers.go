package model

// BuiltinProviders returns inference endpoints selected by Core. Catalog sources
// cannot change these destinations. A host can supply a separate trusted Provider.
func BuiltinProviders() []Provider {
	return []Provider{
		{ID: "openai", Protocol: Chat, Endpoint: "https://api.openai.com/v1"},
		{ID: "anthropic", Protocol: Messages, Endpoint: "https://api.anthropic.com"},
		{ID: "google", Protocol: Gemini, Endpoint: "https://generativelanguage.googleapis.com"},
		{ID: "deepseek", Protocol: Chat, Endpoint: "https://api.deepseek.com"},
		{ID: "alibaba", Protocol: Chat, Endpoint: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"},
		{ID: "zai", Protocol: Chat, Endpoint: "https://api.z.ai/api/paas/v4"},
		{ID: "moonshotai", Protocol: Chat, Endpoint: "https://api.moonshot.ai/v1"},
		{ID: "minimax", Protocol: Chat, Endpoint: "https://api.minimax.io/v1"},
	}
}
