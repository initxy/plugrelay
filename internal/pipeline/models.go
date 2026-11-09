package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// ServeModels lists the models visible to the caller in the format the
// request asks for. The ETag is a hash of the body; a matching
// If-None-Match gets 304.
func (p *Pipeline) ServeModels(w http.ResponseWriter, r *http.Request, codec protocol.Codec, caller *plugin.Caller) {
	snap := p.snapshot()
	if caller == nil {
		caller = &plugin.Caller{}
	}
	models := make([]ir.ModelInfo, 0, len(snap.Models))
	for _, m := range snap.Models {
		if snap.Visible == nil || snap.Visible(caller, m.ID) {
			models = append(models, m)
		}
	}
	ctype, body := codec.EncodeModels(r, models)
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	if etagMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", ctype)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func etagMatch(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}

// ServeHello answers the liveness probe some clients send at startup.
func (p *Pipeline) ServeHello(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
