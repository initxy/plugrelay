package convert

import (
	"strconv"

	"github.com/initxy/plugrelay/pkg/ir"
)

// assessMedia grades a media request, or a text request for a media-only
// endpoint: the endpoint must serve the operation, and token-id embedding
// input needs a protocol that takes token ids.
func assessMedia(needs ir.Needs, c *Context) (ir.Fidelity, []string) {
	t := c.Target
	var missing []string
	if !t.Serves(needs.Modality) {
		m := string(needs.Modality)
		if m == "" {
			m = "text_generation"
		}
		missing = append(missing, m)
	}
	if needs.EmbedTokens && !t.EmbedTokens {
		missing = append(missing, "embed_tokens")
	}
	if len(missing) > 0 {
		return ir.Impossible, missing
	}
	return ir.Lossless, nil
}

// stepEmbed records what an embeddings conversion does: base64 output
// encoded by the relay, several inputs sent as one batch. Retrieval hints
// and the end-user tag are dropped by codecs whose protocol has no slot for
// them, unrecorded.
func stepEmbed(req *ir.Request, c *Context, r *recorder) {
	e := req.Embed
	if e == nil || c.SameProtocol {
		return
	}
	if e.Base64 {
		r.note("embeddings: base64 encoded by the relay")
	}
	if len(e.Inputs) > 1 {
		r.step("embeddings: " + strconv.Itoa(len(e.Inputs)) + " inputs sent as one batch")
	}
}

// stepRerank records what a rerank conversion approximates: object
// documents go to a string-only protocol as their text, and the relay
// returns documents and applies top_n when the target does not.
func stepRerank(req *ir.Request, c *Context, r *recorder) {
	rr := req.Rerank
	if rr == nil || c.SameProtocol {
		return
	}
	for _, d := range rr.Documents {
		if d.Object != nil {
			r.note("rerank: object documents may be sent as their text")
			break
		}
	}
	r.note("rerank: documents and top_n applied by the relay when the upstream omits them")
}
