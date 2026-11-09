package convert

import (
	"encoding/json"
	"slices"

	"github.com/initxy/plugrelay/internal/seal"
	"github.com/initxy/plugrelay/pkg/ir"
)

// HistoryIssuers lists, sorted and unique, the issuers of the opaque states
// in a request's history, for Context.HistoryIssuers. Raw (unsealed) state
// is listed as ""; seals that fail to open are left out, as they are dropped
// on every endpoint alike. The result is non-nil.
func HistoryIssuers(req *ir.Request, s *seal.Sealer) []string {
	out := []string{}
	add := func(st *ir.OpaqueState) {
		iss := ""
		if st.Sealed {
			opened, ok := openState(st, s)
			if !ok {
				return
			}
			iss = opened.Issuer
		}
		if !slices.Contains(out, iss) {
			out = append(out, iss)
		}
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Opaque != nil {
				add(b.Opaque)
			}
		}
	}
	slices.Sort(out)
	return out
}

// keepsIssuer reports whether state of this issuer may go to the target.
// Raw state is accepted only on the same protocol, and only when its issuer
// is unknown or matches; sealed state only when its issuer matches.
func (c *Context) keepsIssuer(issuer string, raw bool) bool {
	if raw {
		return c.SameProtocol && (issuer == "" || issuer == c.TargetIssuer)
	}
	return issuer == c.TargetIssuer
}

// openState unseals a Sealed state. Data holds the seal as a JSON string.
func openState(st *ir.OpaqueState, s *seal.Sealer) (ir.OpaqueState, bool) {
	var str string
	if err := json.Unmarshal(st.Data, &str); err != nil {
		str = string(st.Data)
	}
	opened, err := s.Open(str)
	if err != nil {
		return ir.OpaqueState{}, false
	}
	return opened, true
}

// resolve decides the fate of one history state: the state to send, or nil
// to drop it.
func (c *Context) resolve(st *ir.OpaqueState) *ir.OpaqueState {
	if !st.Sealed {
		if c.keepsIssuer(st.Issuer, true) {
			return st
		}
		return nil
	}
	opened, ok := openState(st, c.Sealer)
	if !ok || !c.keepsIssuer(opened.Issuer, false) {
		return nil
	}
	return &opened
}

// stepOpaque returns upstream-private state only to its issuer. Dropped
// state never fails the request. A thinking block whose signature is dropped
// is dropped whole, since issuers that sign thinking reject it unsigned and
// others gain nothing from it; raw items survive only the same protocol.
// Messages left empty are removed and neighbors of the same role merged.
func stepOpaque(req *ir.Request, c *Context, r *recorder) {
	droppedState, droppedRaw, removed := 0, 0, false
	for i := range req.Messages {
		m := &req.Messages[i]
		out := m.Content[:0:0]
		for _, b := range m.Content {
			if b.Type == ir.BlockRaw && !c.SameProtocol {
				droppedRaw++
				continue
			}
			if b.Opaque != nil {
				st := c.resolve(b.Opaque)
				if st == nil {
					droppedState++
					if b.Type == ir.BlockOpaque || b.Type == ir.BlockThinking {
						continue
					}
				}
				b.Opaque = st
			}
			out = append(out, b)
		}
		if len(out) != len(m.Content) {
			removed = true
		}
		m.Content = out
	}
	if droppedState > 0 {
		r.step("opaque: state from other issuers dropped")
		r.drop("opaque_state")
		r.major(lossOpaque)
	}
	if droppedRaw > 0 {
		r.step("opaque: raw items dropped")
		r.drop("raw_items")
	}
	if removed {
		req.Messages = compactMessages(req.Messages)
	}
}

func compactMessages(msgs []ir.Message) []ir.Message {
	out := msgs[:0:0]
	for _, m := range msgs {
		if len(m.Content) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			out[n-1].Content = append(slices.Clip(out[n-1].Content), m.Content...)
			continue
		}
		out = append(out, m)
	}
	return out
}

// canEcho reports whether upstream state can reach the client and come back.
func (c *Context) canEcho() bool {
	return c.SameProtocol || (len(c.Source.EchoFields) > 0 && c.Sealer != nil)
}

// outbound prepares upstream state for the client: tagged with the target's
// issuer, and sealed unless the client speaks the upstream's protocol. Nil
// means it cannot travel and is dropped.
func (c *Context) outbound(st *ir.OpaqueState) *ir.OpaqueState {
	cp := *st
	cp.Issuer = c.TargetIssuer
	cp.Sealed = false
	if c.SameProtocol {
		return &cp
	}
	if !c.canEcho() {
		return nil
	}
	s, err := c.Sealer.Seal(cp)
	if err != nil {
		return nil
	}
	data, _ := json.Marshal(s)
	return &ir.OpaqueState{Issuer: cp.Issuer, Kind: cp.Kind, Data: data, Sealed: true}
}
