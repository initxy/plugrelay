package admin

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"slices"
	"strings"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// apiKeyPlaceholder stands for the key secret, which the relay cannot show
// after creation.
const apiKeyPlaceholder = "{{api_key}}"

type snippet struct {
	Title    plugin.Text `json:"title"`
	Path     string      `json:"path,omitempty"`
	Language string      `json:"language"`
	Content  string      `json:"content"`
}

type connectClient struct {
	Client      string        `json:"client"`
	Name        string        `json:"name"`
	OS          string        `json:"os"`
	Snippets    []snippet     `json:"snippets"`
	CCSwitchURL string        `json:"ccswitch_url,omitempty"`
	Notes       []plugin.Text `json:"notes,omitempty"`
}

type commonError struct {
	Symptom string      `json:"symptom"`
	Fix     plugin.Text `json:"fix"`
}

func (h *Handler) getKeyConnect(c *call) error {
	k, err := h.ownKey(c, c.r.PathValue("id"))
	if err != nil {
		return err
	}
	osName := c.query("os")
	if osName == "" {
		osName = "macos"
	}
	if !slices.Contains([]string{"macos", "linux", "windows"}, osName) {
		return fieldInvalid("os", oneOf("macos", "linux", "windows"))
	}
	model := ""
	if ms := readKeySettings(k).Models; len(ms) > 0 && !strings.ContainsAny(ms[0], "*?[") {
		model = ms[0]
	}
	base := h.baseURL(c)
	return c.json(200, map[string]any{
		"base_url":      base,
		"placeholder":   apiKeyPlaceholder,
		"clients":       connectClients(base, osName, model, h.siteName(c.ctx)),
		"common_errors": commonErrors(),
	})
}

// envBlock renders environment exports for a shell of osName.
func envBlock(osName string, kv [][2]string) snippet {
	var b strings.Builder
	if osName == "windows" {
		for _, p := range kv {
			b.WriteString("$env:" + p[0] + " = \"" + p[1] + "\"\n")
		}
		return snippet{Title: plugin.Text{EN: "PowerShell"}, Language: "powershell", Content: b.String()}
	}
	for _, p := range kv {
		b.WriteString("export " + p[0] + "=\"" + p[1] + "\"\n")
	}
	rc := "~/.zshrc"
	if osName == "linux" {
		rc = "~/.bashrc"
	}
	return snippet{Title: plugin.Text{EN: rc}, Path: rc, Language: "bash", Content: b.String()}
}

func connectClients(base, osName, model, site string) []connectClient {
	key := apiKeyPlaceholder
	claudeEnv := [][2]string{{"ANTHROPIC_BASE_URL", base}, {"ANTHROPIC_AUTH_TOKEN", key},
		{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1"}}
	if model != "" {
		claudeEnv = append(claudeEnv, [2]string{"ANTHROPIC_MODEL", model})
	}
	claudeSettings, _ := json.MarshalIndent(map[string]any{"env": envMap(claudeEnv)}, "", "  ")

	codexModel := model
	if codexModel == "" {
		codexModel = "gpt-5.5"
	}
	// Codex reads model metadata for custom providers only from a local
	// file (model_catalog_json), so the snippet downloads the relay's
	// catalog first. Codex will not start if that file is missing.
	codexToml := "model = \"" + codexModel + "\"\nmodel_provider = \"plugrelay\"\n" +
		"model_catalog_json = \"~/.codex/plugrelay-models.json\"\n\n" +
		"[model_providers.plugrelay]\nname = \"plugrelay\"\nbase_url = \"" + base + "/v1\"\n" +
		"env_key = \"PLUGRELAY_API_KEY\"\nwire_api = \"responses\"\n"
	catalogURL := base + "/v1/models?client_version=0.166.0"
	catalogTitle := plugin.T("Model catalog", "模型清单")
	codexCatalog := snippet{Title: catalogTitle, Language: "bash",
		Content: "mkdir -p ~/.codex && curl -fsS -H \"Authorization: Bearer " + key + "\" \\\n  '" + catalogURL + "' -o ~/.codex/plugrelay-models.json\n"}
	if osName == "windows" {
		codexCatalog = snippet{Title: catalogTitle, Language: "powershell",
			Content: "New-Item -ItemType Directory -Force \"$HOME\\.codex\" | Out-Null\n" +
				"Invoke-WebRequest -Headers @{Authorization=\"Bearer " + key + "\"} -Uri '" + catalogURL +
				"' -OutFile \"$HOME\\.codex\\plugrelay-models.json\"\n"}
	}

	openCode, _ := json.MarshalIndent(map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{"plugrelay": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "plugrelay",
			"options": map[string]any{"baseURL": base + "/v1", "apiKey": key},
			"models":  map[string]any{orDefault(model, "gpt-5.5"): map[string]any{}},
		}},
	}, "", "  ")

	curlModel := orDefault(model, "gpt-5.5")
	curl := "curl " + base + "/v1/chat/completions \\\n  -H \"Authorization: Bearer " + key + "\" \\\n" +
		"  -H \"Content-Type: application/json\" \\\n  -d '{\"model\":\"" + curlModel +
		"\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'\n"
	if osName == "windows" {
		curl = "curl.exe " + base + "/v1/chat/completions -H \"Authorization: Bearer " + key +
			"\" -H \"Content-Type: application/json\" -d '{\\\"model\\\":\\\"" + curlModel +
			"\\\",\\\"messages\\\":[{\\\"role\\\":\\\"user\\\",\\\"content\\\":\\\"hello\\\"}]}'\n"
	}

	return []connectClient{
		{Client: "claude-code", Name: "Claude Code", OS: osName,
			Snippets: []snippet{envBlock(osName, claudeEnv),
				{Title: plugin.Text{EN: "~/.claude/settings.json"}, Path: "~/.claude/settings.json", Language: "json", Content: string(claudeSettings) + "\n"}},
			CCSwitchURL: ccSwitch{app: "claude", name: site, endpoint: base, model: model, format: "anthropic",
				config: `{"env":{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1"}}`, configFormat: "json"}.url(),
			Notes: []plugin.Text{plugin.T("Or run: plugrelay setup claude-code --url "+base+" --key "+key,
				"也可以直接运行：plugrelay setup claude-code --url "+base+" --key "+key)}},
		{Client: "codex", Name: "Codex", OS: osName,
			Snippets: []snippet{
				codexCatalog,
				{Title: plugin.Text{EN: "~/.codex/config.toml"}, Path: "~/.codex/config.toml", Language: "toml", Content: codexToml},
				envBlock(osName, [][2]string{{"PLUGRELAY_API_KEY", key}}),
			},
			CCSwitchURL: ccSwitch{app: "codex", name: site, endpoint: base + "/v1", model: codexModel, format: "openai_responses",
				config: "model_provider = \"plugrelay\"\nmodel = \"" + codexModel + "\"\n\n[model_providers.plugrelay]\nname = \"" + site +
					"\"\nbase_url = \"" + base + "/v1\"\nwire_api = \"responses\"\nsupports_websockets = false\n", configFormat: "toml"}.url(),
			Notes: []plugin.Text{
				plugin.T("Use the custom provider; WebSocket and remote compaction stay off.",
					"用自定义服务商的方式接入；WebSocket 和远程压缩保持关闭。"),
				plugin.T("The key must be allowed at least one model: Codex will not start with an empty model catalog.",
					"这把密钥至少要能用一个模型：模型清单为空时 Codex 启动不了。"),
				plugin.T("Download the model catalog again after models are added; or run: plugrelay setup codex --url "+base+" --key "+key,
					"加了新模型后要重新下载模型清单；也可以直接运行：plugrelay setup codex --url "+base+" --key "+key)}},
		{Client: "gemini-cli", Name: "Gemini CLI", OS: osName,
			Snippets:    []snippet{envBlock(osName, [][2]string{{"GOOGLE_GEMINI_BASE_URL", base}, {"GEMINI_API_KEY", key}})},
			CCSwitchURL: ccSwitch{app: "gemini", name: site, endpoint: base, model: model, format: "gemini_native"}.url()},
		{Client: "cursor", Name: "Cursor", OS: osName,
			Snippets: []snippet{{Title: plugin.T("Settings → Models", "设置 → Models"), Language: "text",
				Content: "Override OpenAI Base URL: " + base + "/v1\nOpenAI API Key: " + key + "\n"}},
			Notes: []plugin.Text{plugin.T("Cursor calls from its own servers: the relay needs a public HTTPS address.",
				"Cursor 是从它自己的服务器发请求的：中转必须有公网 HTTPS 地址。")}},
		{Client: "cline", Name: "Cline", OS: osName,
			Snippets: []snippet{{Title: plugin.T("Provider settings", "服务商设置"), Language: "text",
				Content: "API Provider: OpenAI Compatible\nBase URL: " + base + "/v1\nAPI Key: " + key + "\n"}}},
		{Client: "opencode", Name: "OpenCode", OS: osName,
			Snippets: []snippet{{Title: plugin.Text{EN: "~/.config/opencode/opencode.json"}, Path: "~/.config/opencode/opencode.json",
				Language: "json", Content: string(openCode) + "\n"}}},
		{Client: "curl", Name: "curl", OS: osName,
			Snippets: []snippet{{Title: plugin.T("Test request", "测试请求"), Language: shellLang(osName), Content: curl}}},
	}
}

func shellLang(osName string) string {
	if osName == "windows" {
		return "powershell"
	}
	return "bash"
}

func envMap(kv [][2]string) map[string]string {
	m := map[string]string{}
	for _, p := range kv {
		m[p[0]] = p[1]
	}
	return m
}

// ccSwitch is a CC Switch provider import (its ccswitch://v1/import deep
// link, resource=provider): app is claude, codex or gemini; format the API
// format the app talks to the relay; config, when set, is the app's own
// configuration CC Switch merges (JSON for Claude Code, config.toml for Codex).
type ccSwitch struct {
	app, name, endpoint, model, format string
	config, configFormat               string
}

// url renders the link. The key placeholder stays literal so the console
// can put the secret in (URL-escaped) when it knows it.
func (l ccSwitch) url() string {
	q := url.Values{}
	q.Set("resource", "provider")
	q.Set("app", l.app)
	q.Set("name", l.name)
	q.Set("endpoint", l.endpoint)
	q.Set("homepage", l.endpoint)
	q.Set("apiKey", apiKeyPlaceholder)
	q.Set("apiFormat", l.format)
	if l.model != "" {
		q.Set("model", l.model)
	}
	if l.config != "" {
		q.Set("config", base64.StdEncoding.EncodeToString([]byte(l.config)))
		q.Set("configFormat", l.configFormat)
	}
	enc := strings.ReplaceAll(q.Encode(), url.QueryEscape(apiKeyPlaceholder), apiKeyPlaceholder)
	return "ccswitch://v1/import?" + enc
}

func commonErrors() []commonError {
	return []commonError{
		{Symptom: "401 invalid x-api-key / invalid API key", Fix: plugin.T("The key is wrong, revoked or rotated; copy it again.",
			"密钥不对、已删除或已换新；重新复制一次。")},
		{Symptom: "404 model not found", Fix: plugin.T("The model is not enabled for this key; pick one from /v1/models.",
			"这把密钥不能用这个模型；从 /v1/models 里挑一个能用的。")},
		{Symptom: "429 quota exhausted", Fix: plugin.T("The key's budget is used up; wait for the reset time or raise the budget.",
			"密钥的预算用完了；等到重置时间，或者调高预算。")},
		{Symptom: "Connection refused", Fix: plugin.T("Check the base URL; a relay listening on 127.0.0.1 is only reachable from this machine.",
			"检查地址；只监听 127.0.0.1 的中转只有本机能连。")},
	}
}
