// Package anthropic implements the Anthropic Messages protocol codec
// (POST /v1/messages, /v1/messages/count_tokens, GET /v1/models).
//
// Decoding keeps every wire detail a same-protocol round trip needs: the
// system array and its order, cache_control with ttl, thinking signatures and
// redacted_thinking as opaque state, server tool blocks verbatim, and any
// field the IR has no slot for in Extra with Origin "anthropic". Shapes the
// IR normalises away (string versus array content, partly modelled objects)
// are recorded under codec-private Extra keys starting with "$", which never
// reach the wire.
//
// Importing the package registers the codec with package protocol.
package anthropic
