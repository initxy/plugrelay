// Some npm packages ship .go files; a go.mod here makes `go build ./...`
// and `go vet ./...` in the repo root skip node_modules entirely.
import { existsSync, writeFileSync } from "node:fs";

if (existsSync("node_modules")) {
  writeFileSync("node_modules/go.mod", "module ignore\n");
}
