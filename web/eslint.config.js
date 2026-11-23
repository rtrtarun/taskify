import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["dist", "node_modules", "playwright-report", "test-results", "src/api/schema.d.ts"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["**/*.{ts,tsx}"],
    languageOptions: { globals: { ...globals.browser, ...globals.node } },
    plugins: { "react-hooks": reactHooks },
    rules: {
      "react-hooks/rules-of-hooks": "error",
      "react-hooks/exhaustive-deps": "error",
      // Task and comment text is always text, never HTML.
      "no-restricted-properties": ["error", { property: "innerHTML", message: "Render text, never HTML." }],
    },
  },
  {
    // Rendering text through dangerouslySetInnerHTML is banned everywhere.
    files: ["src/**/*.tsx"],
    rules: { "no-restricted-syntax": ["error", { selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']", message: "Render text, never HTML." }] },
  },
);
