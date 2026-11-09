package pipeline

import (
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// checkResponse runs the response hooks over a complete response before it
// is sent. It reports whether a hook edited r, or the error to answer with.
func (x *exchange) checkResponse(r *ir.Response) (bool, *ir.GwError) {
	edited := false
	for _, pl := range x.entered {
		h, ok := pl.(plugin.ResponseHook)
		if !ok || !supports(pl, "middleware.onResponse") {
			continue
		}
		info := pl.Info()
		e, err := h.OnResponse(x.call.WithPlugin(info.ID), r)
		if err != nil {
			if gw, stop := x.hookError(info, "response", err); stop {
				return edited, gw
			}
			continue
		}
		edited = edited || e
	}
	return edited, nil
}

// refuseResponse ends an attempt whose complete response a hook refused:
// the upstream answered (its cost is recorded) but the client gets the
// hook's error and the attempt is neither delivered nor retried.
func (x *exchange) refuseResponse(a *attempt, gw *ir.GwError) result {
	a.errMsg = gw.Message
	return result{next: nextStop, fail: &failure{err: gw}}
}

// attachNotes appends the plugins' notes to the record's delivered attempt,
// else its last one.
func (x *exchange) attachNotes(rec *plugin.Record) {
	if x.call == nil || len(rec.Attempts) == 0 {
		return
	}
	notes := x.call.Notes()
	if len(notes) == 0 {
		return
	}
	i := len(rec.Attempts) - 1
	for j, a := range x.attempts {
		if a == x.delivered && j < len(rec.Attempts) {
			i = j
		}
	}
	d := ir.Diagnostic{Fidelity: ir.Lossless}
	if old := rec.Attempts[i].Diagnostic; old != nil {
		d = *old
		d.Notes = append([]string(nil), old.Notes...)
	}
	d.Notes = append(d.Notes, notes...)
	rec.Attempts[i].Diagnostic = &d
}
