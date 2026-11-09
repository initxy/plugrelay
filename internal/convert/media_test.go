package convert

import (
	"slices"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
)

func TestAssessMedia(t *testing.T) {
	chat := ir.Capabilities{FunctionTools: true, Images: true}
	embed := ir.Capabilities{MediaOnly: true, Embeddings: true}
	embedTokens := ir.Capabilities{MediaOnly: true, Embeddings: true, EmbedTokens: true}
	audio := ir.Capabilities{MediaOnly: true, AudioTranscription: true}
	cases := []struct {
		name    string
		needs   ir.Needs
		target  ir.Capabilities
		want    ir.Fidelity
		missing string
	}{
		{"text to media-only", ir.Needs{}, embed, ir.Impossible, "text_generation"},
		{"embeddings to chat", ir.Needs{Modality: ir.ModalityEmbeddings}, chat, ir.Impossible, "embeddings"},
		{"embeddings", ir.Needs{Modality: ir.ModalityEmbeddings}, embed, ir.Lossless, ""},
		{"token ids without token input", ir.Needs{Modality: ir.ModalityEmbeddings, EmbedTokens: true}, embed, ir.Impossible, "embed_tokens"},
		{"token ids", ir.Needs{Modality: ir.ModalityEmbeddings, EmbedTokens: true}, embedTokens, ir.Lossless, ""},
		{"speech on a transcription-only endpoint", ir.Needs{Modality: ir.ModalitySpeech}, audio, ir.Impossible, "speech"},
		{"transcription", ir.Needs{Modality: ir.ModalityTranscription}, audio, ir.Lossless, ""},
		{"image generation to chat", ir.Needs{Modality: ir.ModalityImageGeneration}, chat, ir.Impossible, "image_generation"},
	}
	for _, tc := range cases {
		f, missing := Assess(tc.needs, &Context{Target: tc.target})
		if f != tc.want || (tc.missing != "" && !slices.Contains(missing, tc.missing)) {
			t.Errorf("%s: %s %v", tc.name, f, missing)
		}
	}
}

func TestRequestEmbedSkipsTextSteps(t *testing.T) {
	req := &ir.Request{Model: "m", Embed: &ir.Embed{Inputs: []ir.EmbedInput{{Text: "a longer text"}, {Text: "b"}}, Base64: true}}
	c := &Context{Source: ir.Capabilities{MediaOnly: true, Embeddings: true, EmbedTokens: true},
		Target: ir.Capabilities{MediaOnly: true, Embeddings: true, Cache: ir.CacheKeyed}, SessionID: "s"}
	d, err := Request(req, ir.Needs{Modality: ir.ModalityEmbeddings}, c)
	if err != nil || d.Fidelity != ir.Lossless {
		t.Fatalf("%+v %v", d, err)
	}
	if req.CacheKey != "" || len(req.Messages) != 0 {
		t.Errorf("text steps ran: %+v", req)
	}
	if len(d.Notes) == 0 || len(d.Steps) == 0 {
		t.Errorf("diagnostic %+v", d)
	}
	if u := EstimateUsage(req, 0); u.Input == 0 || !u.Estimated {
		t.Errorf("estimate %+v", u)
	}
}
