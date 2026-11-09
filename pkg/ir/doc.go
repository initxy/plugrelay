// Package ir defines plugrelay's protocol-neutral intermediate representation.
//
// Every wire protocol (Anthropic messages, OpenAI responses, chat and
// completions, Gemini, and the media protocols: embeddings, images, audio,
// rerank) has exactly one codec that maps between its wire format and these
// types. The IR is the union of what those protocols can express; its shape is
// closest to Anthropic content blocks, and streaming is unified into Events.
//
// Codecs move fields, they never decide what to drop: anything the IR cannot
// represent is kept verbatim in an Extra map so a round trip through the same
// protocol is lossless. Deciding what to do when the target lacks a capability
// is the job of the generic conversion steps, not of codecs.
package ir
