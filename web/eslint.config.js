import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import globals from "globals";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["dist", "node_modules", "src/lib/api/schema.gen.ts"] },
  {
    files: ["**/*.{ts,tsx}"],
    extends: [js.configs.recommended, ...tseslint.configs.strict],
    languageOptions: { ecmaVersion: 2023, globals: globals.browser },
    plugins: { "react-hooks": reactHooks, "react-refresh": reactRefresh },
    rules: {
      ...reactHooks.configs.recommended.rules,
      // Feature modules export a component next to its pure helpers (and tests import those);
      // fast refresh falls back to a full reload there, which is fine.
      "react-refresh/only-export-components": "off",
      // We don't run the React Compiler; TanStack Table is used as documented.
      "react-hooks/incompatible-library": "off",
      "@typescript-eslint/consistent-type-imports": "error",
      "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_", varsIgnorePattern: "^_", ignoreRestSiblings: true }],
      // With noUncheckedIndexedAccess, `!` is the explicit "checked by hand" marker.
      "@typescript-eslint/no-non-null-assertion": "off",
      "@typescript-eslint/no-dynamic-delete": "off",
      "no-restricted-imports": [
        "error",
        { paths: [{ name: "lucide-react", importNames: ["icons"], message: "Import icons one by one." }] },
      ],
    },
  },
);
