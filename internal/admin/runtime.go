package admin

import (
	"github.com/initxy/plugrelay/internal/extplugin"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// TestRequest narrows an upstream test.
type TestRequest struct {
	Model        string `json:"model,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
	EndpointID   string `json:"endpoint_id,omitempty"`
}

// TestResult is the outcome of one test request.
type TestResult struct {
	OK           bool   `json:"ok"`
	UpstreamID   string `json:"upstream_id,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
	EndpointID   string `json:"endpoint_id,omitempty"`
	Model        string `json:"model,omitempty"`
	Status       int    `json:"status,omitempty"`
	TTFBMs       int64  `json:"ttfb_ms,omitempty"`
	LatencyMs    int64  `json:"latency_ms"`
	Error        string `json:"error,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
}

// Passed reports whether the test succeeded.
func (r TestResult) Passed() bool { return r.OK }

// SyncFailure is one upstream a model sync could not read.
type SyncFailure struct {
	UpstreamID string `json:"upstream_id"`
	Error      string `json:"error"`
}

// SyncResult summarizes a model sync.
type SyncResult struct {
	Added     []string      `json:"added"`
	Disabled  []string      `json:"disabled"`
	Unchanged int           `json:"unchanged"`
	Failed    []SyncFailure `json:"failed"`
}

// ManifestInfo is an upstream manifest available to create from.
type ManifestInfo struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	TitleZh  string `json:"title_zh,omitempty"`
	Category string `json:"category"`
	Kind     string `json:"kind"` // official | file
	// KeyOptional: the server may run without a key (auth.key_optional).
	KeyOptional bool     `json:"key_optional"`
	Version     string   `json:"version,omitempty"`
	Protocols   []string `json:"protocols"`
	DocsURL     string   `json:"docs_url,omitempty"`
	// Variables are the values an upstream sets in its config, by name.
	Variables []ManifestVariable `json:"variables"`
	// Provider is the provider plugin id the manifest runs on.
	Provider string `json:"-"`
}

// ManifestVariable is one manifest variable. Required means an upstream
// must set it: the manifest requires it and gives no default.
type ManifestVariable struct {
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	DescriptionZh string `json:"description_zh,omitempty"`
	Default       string `json:"default,omitempty"`
	Required      bool   `json:"required"`
}

// Plugin sources: compiled in, or an external process.
const (
	SourceBuiltin  = "builtin"
	SourceExternal = "external"
)

// PluginEntry is one installed plugin as the runtime sees it.
type PluginEntry struct {
	Info    plugin.Info
	Source  string
	Enabled bool
	Order   int
	// Status is running, stopped, crashed or needs_approval.
	Status       string
	StatusDetail string
	// Approved lists approved permission ids.
	Approved []string
	// Manifest is set for external plugins.
	Manifest *extplugin.Manifest
	// Routes are the plugin's admin routes (plugin.Admin), if any.
	Routes []plugin.AdminRoute
}
