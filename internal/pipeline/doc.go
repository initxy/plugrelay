// Package pipeline is the single request path of the relay: every client
// protocol and every upstream goes through ServeProtocol, so logging,
// billing, retries and error rendering happen in exactly one place.
//
// One request runs these steps:
//
//  1. Sniff the routing facts (model, stream, session hints) from the raw
//     body and resolve the session id: client session header, then the
//     body's cache key or user tag, then a hash of key id, model and the
//     conversation's stable prefix. A sub-agent id is appended.
//  2. Decode the request once into IR to learn its needs (passthrough still
//     needs them to grade endpoints); the decoded request is read-only.
//  3. Run the request hooks in configured order (FailPolicy on error, a
//     Reply short-circuits), then reserve with the meter.
//  4. Plan candidates with the router, each endpoint graded by
//     convert.Assess against its effective capabilities (codec ∩ endpoint),
//     and let the selector reorder them.
//  5. Try candidates until one succeeds. After a failure the router is asked
//     again with every failed candidate excluded. Whether to retry comes only
//     from the provider's Verdict, and nothing is retried once a content
//     event reached the client. Each attempt picks the lowest tier:
//     passthrough (body bytes untouched), patch (named fields edited with
//     sjson: model, stale private state, output-limit name, declared plugin
//     edits) or convert (IR conversion and re-encoding).
//  6. Stream back as events arrive. Output before the first content event is
//     held so a retry can start clean; keepalives (which are not content)
//     start after KeepaliveDelay without upstream output and repeat every
//     Keepalive while idle. A stream that ends without its protocol's
//     terminal event gets the client protocol's error event, never a clean
//     end.
//  7. Finish: cost every attempt that reached an upstream (estimated when
//     the upstream reported no usage), charge only the delivered attempt,
//     settle or refund exactly once, bind the session for stickiness, run
//     finish hooks in reverse for the plugins whose request hook ran, and
//     queue the record for sinks.
//
// Errors the relay produces itself are always rendered by the client codec
// in its native format; errors of a same-protocol upstream pass through
// verbatim.
package pipeline
