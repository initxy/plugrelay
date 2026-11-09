package server

import (
	"net/http"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// Batch serves the batch APIs (OpenAI files and batches, Anthropic
// message batches); *pipeline.Pipeline implements it. BatchFamily reports
// whether a path belongs to them and which codec renders its errors.
type Batch interface {
	BatchFamily(path string) (codec string, ok bool)
	ServeBatch(w http.ResponseWriter, r *http.Request, caller *plugin.Caller)
}

// serveBatch authenticates a batch API request and hands it on. It runs
// without a write deadline: uploads and result downloads may be long.
func (s *Server) serveBatch(w http.ResponseWriter, r *http.Request, codecName string) {
	caller, gw := s.authenticate(r)
	if gw != nil {
		if c, ok := protocol.Get(codecName); ok {
			writeCodecError(w, c, gw, 0)
		} else {
			writeJSONError(w, http.StatusUnauthorized, string(gw.Kind), gw.Message)
		}
		return
	}
	s.o.Batch.ServeBatch(w, r, caller)
}
