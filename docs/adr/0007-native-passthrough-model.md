# ADR-0007: `/native` is an opt-in raw passthrough

- Status: accepted
- Date: 2026-11-05

## Context

Files, batches and vendor-only APIs have no place in the neutral IR, but
users still want them behind the relay's keys, credential pool and logs.
A raw forward hands the caller everything the upstream credential can do
on that host, not just model calls.

## Decision

- `/native/<upstream>[:<endpoint>]/<path>` forwards method, path, query and
  body unchanged, after platform-key auth, credential selection through the
  router and the provider's auth injection, over core egress.
- Off twice by default: `native.enabled` in relay.yaml, then a per
  group/user/key grant (`middleware.access` `native_upstreams`, merged
  Override so a grant can be scoped down to one key).
- The platform key is never forwarded (credential headers dropped, `?key=`
  stripped); path segments that resolve to `.` or `..` in any encoding are
  refused, so a base URL's path prefix holds.
- No request plugins and no metering run (usage is unknown); every request
  is logged with status, bytes and duration.

## Consequences

- Operators must treat a native grant like handing over the credential.
- An external authenticator that reads the platform key from another
  header or parameter must not be combined with native grants.
