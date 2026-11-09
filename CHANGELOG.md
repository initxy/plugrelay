# Changelog

## Unreleased — first public release

plugrelay is an open-source LLM relay for coding clients: Claude Code,
Codex, Gemini CLI, Cursor, Cline, OpenCode and others point at one address.
Same-protocol traffic passes through byte for byte, so prompt caching works
as well as a direct connection; other requests are matched against what the
upstream can do and converted by a few generic steps. Upstreams are YAML
manifests or plugins; the relay ships with a web console, per-request logs
with the actual upstream and processing tier, and an optional sell kit
(balances, payments, plans). See the [README](README.md).

Later entries are the release notes on
[GitHub Releases](https://github.com/initxy/plugrelay/releases)
([how they are written](docs/releasing.md)).
