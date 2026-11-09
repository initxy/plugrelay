# plugrelay

An LLM relay for coding clients: Go server with an embedded React console (`web/`), YAML upstream manifests (`manifests/`), the external plugin protocol and Go SDK (`sdk/`). Start at `README.md` and `docs/architecture.md`.

## Language

- Code, comments, docs (`CONTEXT.md`, ADRs, `docs/specs/`) and commit messages: English. The console ships English and Simplified Chinese with identical keys (`web/src/i18n/`).
- Reply in the language the user writes in.
- User docs come in pairs: `name.md` and `name.zh-CN.md`. A change to one updates the other in the same diff; `scripts/check-docs.sh` compares their sections. The Chinese is everyday Chinese: say what a thing does instead of translating an identifier word by word, and keep the identifier next to it.

## Context

- Read `CONTEXT.md` before touching domain concepts, system boundaries, or stable conventions. A module with its own `AGENTS.md` keeps its concepts there.
- Read `docs/adr/` before revisiting long-term architecture calls; never rely on ADRs marked `superseded`.
- Read `docs/architecture.md` for how a request flows and which package owns each decision.
- Code is the single source of truth for "what is." When docs and code disagree, trust the code and fix the docs.
- One fact, one home: never state the same fact in two docs — link instead. A change that invalidates a `CONTEXT.md` entry or ADR updates it in the same task, not in a later cleanup.
- Write docs lean: an `AGENTS.md` or `CONTEXT.md` line exists only if code can't tell an agent the same thing quickly; point at code instead of restating it. An ADR only for a decision that has shipped and is costly to reverse — never layout, wording, or flows; changing one is a new ADR. Past the budget, split by module into `<module>/AGENTS.md`. `scripts/check-docs.sh` holds the budgets and formats — trim, don't raise them. When unsure, don't write it.
- Code comments carry only constraints the code can't show — no narration, no restating the spec, never a `docs/specs/` reference (one-shot artifacts; the link breaks once archived).

## Task flow

- Before starting, run `check`. Red that isn't yours → report it before anything else.
- Vague request → shape before code: interview one question at a time, each carrying a recommended answer and its reasoning; look up what code and docs can answer, never bring those to the user. Not converged until no decision that would change the implementation path, boundary, or acceptance criteria is open. ("Just recommend" → answer straight, flagging the assumptions made.)
- Shaping red lines: no implementation code and no spec before the design converges; a finished spec doesn't slide into implementation — starting is a separate decision.
- As decisions settle, record them in the spec's `Decisions` — not yet in `CONTEXT.md` or `docs/adr/`: designs churn, and those are distilled at review wrap-up from what survived. One-off details nowhere.
- Finalize the spec per `docs/specs/TEMPLATE.md` at `docs/specs/<YYYY-MM-DD>-<slug>.md` (slug in English kebab-case): `active` if implementation starts now, `proposed` if queued; statuses per the template — a terminal one and the move into `docs/specs/archive/` are one action.
- A change with no open decision (a bug fix with clear expected behavior, a small change inside one module) needs no spec: implement directly and put the why in the commit message.
- Implement against the spec: flip status to `active` on start (and set `owner` in the frontmatter when others may be working in parallel); tick Plan tasks as they complete; log progress, decision changes, and friction (retries / confusion / slow tests) in the spec's Progress log as you go.
- done = every acceptance criterion met + all automated gates green. Both required, no exceptions.
- Before merge, review per `docs/reviewing.md`: gates first — not green → rejected; verdict is mergeable / revise / re-shape; merging archives the spec and distills long-term knowledge.
- Resume across sessions from the spec alone; write no separate handoff doc.

## Verbs

- dev: `make dev` — runs `plugrelay serve` with `examples/relay.yaml`.
- test: `make test` — `go test -race ./...` plus console unit tests. `PLUGRELAY_TEST_PG=postgres://…` / `PLUGRELAY_TEST_MYSQL=user:pw@tcp(host:3306)/db` add the PostgreSQL / MySQL store/state suites.
- lint: `make lint` (`scripts/lint.sh`) — gofmt, go vet, staticcheck, console typecheck + eslint.
- build: `make build` → `bin/plugrelay` (embeds the last console build; `make web` rebuilds it).
- e2e: `make e2e` — `-tags e2e` tests against a real upstream; needs `PLUGRELAY_E2E_*` env.
- check: `scripts/check.sh` (`make check`) — lint, race tests, console test + build, Go build, then `scripts/check-docs.sh` last. CI is defined to run exactly this.
- release: push an annotated `vX.Y.Z` tag → `.github/workflows/release.yml` runs goreleaser (`.goreleaser.yaml`) — approval gate, see below.

## Gates

- Automated gates (required for done): run `scripts/check.sh`. CI (`.github/workflows/ci.yml`) runs the same script with PostgreSQL and MySQL services; locally add those suites with `PLUGRELAY_TEST_PG` / `PLUGRELAY_TEST_MYSQL`. CI-only steps: none.
- Approval gates (a human must sign off before running): release, data migration, data deletion, external publishing.

## Release

- Follow `docs/releasing.md`. Human sign-off = the maintainer pushes the release tag.

## Engineering constraints

- Feature work changes behavior; maintenance work changes structure. Never mix them in one diff.
- Prefer existing patterns; keep changes focused; no unrelated refactors.
- Prefer deep modules: a small interface hiding enough implementation.
- The interface is the test surface; don't introduce a seam without a real need to swap implementations.
- Run verification matched to the change's risk; if you can't verify, say why.
- Name things for what they do (`job_queue`, `approval_check`), never a metaphor (`hands`, `the mind`, `three doors`) — in code, doc terms, and file slugs alike.

## Maintenance

- After a large feature merges, run a scoped gc over the changed area; periodically (weekly is a good default) run a global gc — both per `docs/gc.md`.
- When `check` prints `gc due`, finish the task first, then tell the user and offer a scoped gc.
- Every gc change leaves behavior unchanged; never mix gc into a feature diff.
