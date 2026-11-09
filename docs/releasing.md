# Releasing

A release publishes, on the GitHub release for the tag: `plugrelay` archives
for linux, darwin and windows (amd64, arm64), `checksums.txt` signed keyless
with cosign, and SBOMs per archive; and to `ghcr.io/initxy/plugrelay`,
`:X.Y.Z-amd64` and `:X.Y.Z-arm64` images plus the multi-arch manifests
`:X.Y.Z` and (for non-pre-release tags) `:latest`, signed with cosign. All of
it is defined in [`.goreleaser.yaml`](../.goreleaser.yaml). The Go plugin
SDK (`sdk/go`) is not published by this flow.

Versioning: patch by default; minor / major is the maintainer's explicit call.

The changelog is the GitHub release of each tag ([`CHANGELOG.md`](../CHANGELOG.md)
only covers the first release and points there). Its notes are the curated
changelog from the annotated tag's message body, followed by goreleaser's
list of `feat:` commits (Features) and `fix:` commits (Fixes); commits with
other prefixes are not listed (`release.header` and `changelog` in
`.goreleaser.yaml`).

1. First release only: enable "Private vulnerability reporting" under the
   repository's Settings → Security — the channel `SECURITY.md` names.
2. Changelog: collect user-visible changes since the last release — archived
   specs in `docs/specs/archive/` are the raw material. Curate; don't paste
   commit subjects.
3. Version: the tag is the version; nothing in the tree is bumped. goreleaser
   stamps it into `internal/version.Version` at build time.
4. Run `scripts/check.sh`; all gates green.
5. Human sign-off: the maintainer pushes the annotated release tag `vX.Y.Z`,
   with the curated changelog as the tag message body
   (`git tag -a vX.Y.Z -F notes.txt`: subject line, blank line, changelog).
6. Publish: the tag push runs [`release.yml`](../.github/workflows/release.yml),
   which builds the console and runs `goreleaser release --clean`. It refuses a
   tag whose message has no body (no changelog).
7. Verify the published artifact: install / deploy it fresh and exercise
   what this release changed.
