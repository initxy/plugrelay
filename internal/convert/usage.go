package convert

import "github.com/initxy/plugrelay/pkg/ir"

// charsPerToken is the rough ratio used for estimates.
const charsPerToken = 4

// imageTokens is a flat estimate per image or document.
const imageTokens = 1000

// EstimateUsage guesses usage for a response whose upstream never reported
// it (a cut stream): input from the request as sent, output from the number
// of characters delivered. The result is marked Estimated.
func EstimateUsage(req *ir.Request, outputChars int) ir.Usage {
	chars, media := 0, 0
	var blocks func([]ir.Block)
	blocks = func(bs []ir.Block) {
		for _, b := range bs {
			chars += len(b.Text)
			if b.Media != nil {
				media++
			}
			if b.Tool != nil {
				chars += len(b.Tool.Name) + len(b.Tool.Input) + len(b.Tool.Text)
			}
			if b.Result != nil {
				blocks(b.Result.Content)
			}
		}
	}
	blocks(req.System)
	for _, m := range req.Messages {
		blocks(m.Content)
	}
	for _, t := range req.Tools {
		chars += len(t.Name) + len(t.Description) + len(t.InputSchema) + len(t.Grammar)
	}
	tokens := 0
	if req.Embed != nil {
		for _, in := range req.Embed.Inputs {
			chars += len(in.Text)
			tokens += len(in.Tokens)
		}
	}
	if rr := req.Rerank; rr != nil {
		for _, d := range rr.Documents {
			chars += len(d.Text) + len(rr.Query)
		}
	}
	return ir.Usage{
		Input:     int64(chars/charsPerToken + media*imageTokens + tokens),
		Output:    int64(outputChars / charsPerToken),
		Estimated: true,
	}
}
