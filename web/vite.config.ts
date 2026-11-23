/// <reference types="vitest/config" />
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// API_PROXY_TARGET lets the dev server and `vite preview` reach a Taskify
// server on another port without CORS, as the embedded build would.
const target = process.env.API_PROXY_TARGET ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  server: { proxy: { "/api": target } },
  preview: { proxy: { "/api": target } },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test-setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
