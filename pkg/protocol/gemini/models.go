package gemini

import (
	"encoding/json"
	"net/http"

	"github.com/initxy/plugrelay/pkg/ir"
)

type wireModel struct {
	Name                       string   `json:"name"`
	BaseModelID                string   `json:"baseModelId"`
	Version                    string   `json:"version"`
	DisplayName                string   `json:"displayName"`
	InputTokenLimit            int      `json:"inputTokenLimit"`
	OutputTokenLimit           int      `json:"outputTokenLimit"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	Thinking                   bool     `json:"thinking"`
}

// EncodeModels implements protocol.Codec with the Gemini list format.
func (Codec) EncodeModels(_ *http.Request, models []ir.ModelInfo) (string, []byte) {
	list := make([]wireModel, 0, len(models))
	for _, m := range models {
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		list = append(list, wireModel{
			Name:                       "models/" + m.ID,
			BaseModelID:                m.ID,
			Version:                    "001",
			DisplayName:                name,
			InputTokenLimit:            m.ContextWindow,
			OutputTokenLimit:           m.MaxOutput,
			SupportedGenerationMethods: []string{ActionGenerate, ActionStream, ActionCountTokens},
			Thinking:                   m.Reasoning,
		})
	}
	body, _ := json.Marshal(struct {
		Models []wireModel `json:"models"`
	}{list})
	return "application/json", body
}

// CountTokens implements protocol.Codec with a local estimate.
func (Codec) CountTokens(req *ir.Request) ([]byte, error) {
	return json.Marshal(map[string]int{"totalTokens": estimateTokens(req)})
}

// Per-item token estimates for content whose size is not its byte length.
const (
	tokensPerImage    = 258
	tokensPerDocument = 1000
)

// estimateTokens roughly counts input tokens: four bytes per token of text
// and JSON, fixed costs for media.
func estimateTokens(req *ir.Request) int {
	n := 0
	var blocks func([]ir.Block)
	blocks = func(bs []ir.Block) {
		for _, b := range bs {
			n += len(b.Text)
			switch b.Type {
			case ir.BlockImage:
				n += 4 * tokensPerImage
			case ir.BlockDocument:
				n += 4 * tokensPerDocument
			case ir.BlockToolUse, ir.BlockServerTool:
				if b.Tool != nil {
					n += len(b.Tool.Name) + len(b.Tool.Input) + len(b.Tool.Text)
				}
			case ir.BlockToolResult:
				if b.Result != nil {
					blocks(b.Result.Content)
				}
			}
		}
	}
	blocks(req.System)
	for _, m := range req.Messages {
		blocks(m.Content)
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return (n + 3) / 4
}
