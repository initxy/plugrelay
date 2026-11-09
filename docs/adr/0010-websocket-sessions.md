# ADR-0010: WebSocket sessions are relayed, policed and billed per response

- Status: accepted
- Date: 2026-11-06

## Context

OpenAI Realtime and the Responses WebSocket mode (Codex) are long-lived
sessions, not request/response pairs. A plain byte pipe would bypass what
HTTP requests get: access lists, rate limits, parameter caps, guardrails and
metering.

## Decision

- `/v1/realtime` and the Responses WebSocket are relayed to endpoints that
  declare `realtime` / `responses_websocket`, dialled through core egress
  with the provider's credential; the client's key never travels upstream.
  A credential concurrency slot is held for the session.
- Policy applies per unit of work: the handshake is checked like a request
  (IP, client, model lists; concurrency), and every `response.create` /
  text-bearing client event runs the request plugins (rpm only), body
  mutators and parameter caps. Duplicate JSON keys are refused.
- Metering is per response: reserve before each response (output cap ×
  price), settle on `response.done`; a response cut short by the client,
  the upstream or shutdown is billed from an estimate marked `Estimated`.
- Guardrails see text both ways with the HTTP hold-back window; blocked
  output never reaches the client, including through later retrieval of the
  same item. Audio is not scanned (optionally refused).
- The Responses WebSocket is accepted only when one upstream serves every
  model the key may use; otherwise 426, the only answer Codex falls back on.

## Consequences

- Session limits (duration ≤ the slot TTL, idle, message size, ready
  timeout) are configuration, not best effort.
- Live verification needs a paid OpenAI/Azure account; the relay is tested
  against a scripted upstream.
