# plugrelay console

The web console bundled into the `plugrelay` binary. React 19, Vite, TypeScript (strict),
Tailwind v4, Radix primitives, TanStack Router / Query / Table, Recharts (lazy).

It only calls the public admin API described in [`../api/openapi.yaml`](../api/openapi.yaml);
there are no console-only endpoints.

## Commands

| | |
|-|-|
| `npm ci` | install (also drops a `go.mod` into `node_modules` so `go ./...` skips it) |
| `npm run dev` | console at :5173, proxying `/api`, `/v1`, `/mcp` and `/metrics` to `PLUGRELAY_URL` (default `http://127.0.0.1:8787`, the demo relay) |
| `npm run gen:api` | regenerate `src/lib/api/schema.gen.ts` from the OpenAPI spec (commit the result) |
| `npm run typecheck` / `lint` / `test` | tsc, eslint, vitest |
| `npm run build` | production build into `dist/` |

## Developing against demo data

Run `make demo` from the repo root, then `npm run dev`, and log in as `admin@demo.local` /
`plugrelay-demo` (members: `alice@`, `bob@`, `carol@`, `dave@demo.local`, same password).
The demo store is deleted on exit; keep it with `make demo DEMO_FLAGS='--dir /tmp/prdemo'`, and
add `--days 30` for longer charts. Data the console needs but the demo lacks goes into
`internal/demo/`, never into the console.

## Layout

```
src/app          shell, routing context, the one menu (nav.ts), session queries, ⌘K palette
src/routes       route definitions (code-based TanStack Router; pages are lazy-loaded)
src/features/*   one folder per page area: overview, keys, logs, upstreams, models, users, features, billing (operator/ and member/ pages; its API calls are typed in billing/api.ts), …
src/components   shared pieces (data-table, drawer, stat-tile, status, filter-bar, form-kit, settings-form for feature, extension and group/user/key settings, page); ui/ holds the Radix primitives
src/lib          API client and shared queries, money/format/duration/text helpers, URL search state, form + unsaved-change hooks, theme
src/i18n         zh-CN and en only; en.ts is the source of keys, zh-CN.ts must have exactly the same (tested)
src/styles.css   design tokens (colors light + dark, radius, shadows, type scale) as Tailwind theme variables
src/test         test helpers: api.tsx renders a page against fixtures typed by the OpenAPI document, plain-words.ts checks for raw ids
```

Money is always `{ micro, currency }` and goes through `lib/money.ts`; forms take currency units
(`MoneyInput`). Filters and the open drawer (`?id=`) live in the URL (`lib/use-url-search.ts`).
`npm run dev` serves `/dev/components`, every shared component in both themes; production builds render nothing there.

## Embedding

`embed.go` (`package web`) embeds `dist/` with `//go:embed all:dist` and exposes `FS` and `Dist()`.
A `dist/index.html` is committed so `go build` works before the first console build.
`npm run build` replaces it; don't commit the build output (`git update-index --skip-worktree web/dist/index.html` keeps
`git status` quiet).

## Feature pages

Pages a feature adds (billing, MCP servers, sign-in providers) live at fixed routes and show while
`GET /me` lists their page id in `pages`. `src/app/nav.ts` is the one map from page ids to routes
and menu groups. Extensions cannot add console UI.
