package compatible

import "github.com/initxy/plugrelay/pkg/plugin"

// Issuer groups of the official registrations. Upstreams in different
// groups never see each other's private state, even for the same model.
var (
	OpenAIIssuers    = []string{"openai", "azure-openai"}
	AnthropicIssuers = []string{"anthropic", "bedrock", "vertex-anthropic"}
)

// Plugin ids registered by this package.
const (
	IDOpenAICompatible    = "provider.openai-compatible"
	IDAnthropicCompatible = "provider.anthropic-compatible"
	IDGemini              = "provider.gemini"
	IDOpenAI              = "provider.openai"
	IDAnthropic           = "provider.anthropic"
)

func init() {
	plugin.Register(IDOpenAICompatible, func() plugin.Plugin {
		return New("openai-compatible", WithDescription("Generic OpenAI-compatible upstream driven by a manifest"))
	})
	plugin.Register(IDAnthropicCompatible, func() plugin.Plugin {
		return New("anthropic-compatible", WithDescription("Generic Anthropic-compatible upstream driven by a manifest"))
	})
	plugin.Register(IDGemini, func() plugin.Plugin {
		return New("gemini", WithDefaultManifest("gemini"),
			WithDescription("Gemini API, and interpreter of Gemini-compatible manifests"))
	})
	plugin.Register(IDOpenAI, func() plugin.Plugin {
		return New("openai", WithDefaultManifest("openai"), WithIssuers(OpenAIIssuers...),
			WithDescription("OpenAI and Azure OpenAI (manifest: azure-openai)"))
	})
	plugin.Register(IDAnthropic, func() plugin.Plugin {
		return New("anthropic", WithDefaultManifest("anthropic"), WithIssuers(AnthropicIssuers...),
			WithDescription("Anthropic API; bedrock and vertex-anthropic issuer groups"))
	})
}
