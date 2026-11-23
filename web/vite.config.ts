/// <reference types="vitest/config" />
import react from "@vitejs/plugin-react";
import { readFileSync } from "node:fs";
import { defineConfig } from "vite";

// API_PROXY_TARGET lets the dev server and `vite preview` reach a Taskify
// server on another port without CORS, as the embedded build would.
const target = process.env.API_PROXY_TARGET ?? "http://localhost:8080";

// The contract version this app is built from: info.version of the document
// the API types are generated from. It is read here, not written down again.
function contractVersion(): string {
  const doc = readFileSync(new URL("../contract/openapi.yaml", import.meta.url), "utf8");
  const info = /^info:\r?\n((?:[ #].*\r?\n|\r?\n)*)/m.exec(doc)?.[1] ?? "";
  const v = /^ {2}version:\s*["']?([^"'\s]+)/m.exec(info)?.[1];
  if (!v) throw new Error("contract/openapi.yaml has no info.version");
  return v;
}

export default defineConfig({
  plugins: [react()],
  define: { __API_VERSION__: JSON.stringify(contractVersion()) },
  server: { proxy: { "/api": target } },
  preview: { proxy: { "/api": target } },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test-setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
