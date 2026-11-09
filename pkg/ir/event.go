package ir

// EventKind tags a streaming Event.
type EventKind string

const (
	// EvStart opens the response: ID and Model are set.
	EvStart EventKind = "start"
	// EvBlockStart opens content block Index; Block holds its type and any
	// fields known up front (tool id and name, opaque state).
	EvBlockStart EventKind = "block_start"
	// EvDelta appends to block Index.
	EvDelta EventKind = "delta"
	// EvBlockStop closes block Index. Block may carry state that arrived
	// at the end (a thinking signature, a complete encrypted reasoning item).
	EvBlockStop EventKind = "block_stop"
	// EvUsage reports usage so far; the last one wins.
	EvUsage EventKind = "usage"
	// EvPing is a keepalive. It never counts as content.
	EvPing EventKind = "ping"
	// EvStatus is relay-side progress (queued, position N); never sent upstream.
	EvStatus EventKind = "status"
	// EvDone ends the response normally; StopReason is set.
	EvDone EventKind = "done"
	// EvError ends the response abnormally; Err is set.
	EvError EventKind = "error"
)

// DeltaKind says what a Delta appends.
type DeltaKind string

const (
	DeltaText      DeltaKind = "text"       // Text
	DeltaThinking  DeltaKind = "thinking"   // Text
	DeltaToolInput DeltaKind = "tool_input" // Text: a fragment of JSON or freeform input
	DeltaOpaque    DeltaKind = "opaque"     // Opaque: signature or encrypted state fragment
	DeltaCitation  DeltaKind = "citation"   // Extra: an annotation, verbatim
)

// Delta is an incremental addition to a block.
type Delta struct {
	Kind   DeltaKind    `json:"kind"`
	Text   string       `json:"text,omitempty"`
	Opaque *OpaqueState `json:"opaque,omitempty"`
	Extra  Extra        `json:"extra,omitzero"`
}

// Event is one step of a streamed response. Indexes are dense and start at
// 0; a block is always started before its deltas and stopped exactly once.
type Event struct {
	Kind       EventKind  `json:"kind"`
	Index      int        `json:"index,omitempty"`
	ID         string     `json:"id,omitempty"`    // EvStart
	Model      string     `json:"model,omitempty"` // EvStart
	Block      *Block     `json:"block,omitempty"`
	Delta      *Delta     `json:"delta,omitempty"`
	Usage      *Usage     `json:"usage,omitempty"`
	StopReason StopReason `json:"stop_reason,omitempty"` // EvDone
	StopSeq    string     `json:"stop_sequence,omitempty"`
	Status     string     `json:"status,omitempty"` // EvStatus
	Err        *GwError   `json:"error,omitempty"`  // EvError
}

// IsContent reports whether the event delivers something the client will
// treat as model output. After the first content event the pipeline never
// retries on another upstream.
func (e *Event) IsContent() bool {
	switch e.Kind {
	case EvBlockStart, EvDelta, EvBlockStop, EvDone:
		return true
	}
	return false
}
