# Taskify web

React, TypeScript, Vite, TanStack Query, React Router. API types are generated from `contract/openapi.yaml`; never edit `src/api/schema.d.ts` by hand.

```bash
npm ci
npm run generate    # regenerate src/api/schema.d.ts after a contract change
npm run lint && npm run typecheck && npm test
npm run dev         # proxies /api to API_PROXY_TARGET (default http://localhost:8080)
npm run e2e         # Playwright; see below
```

Task and comment text is always rendered as text; the lint rules ban `innerHTML` and `dangerouslySetInnerHTML`.

## Browser tests

`npm run e2e` starts everything from this checkout (`e2e/global-setup.ts`): a throwaway migrated database made by `tools/e2edb` on `E2E_DATABASE_URL` (default: the compose Postgres on `localhost:5433`; the role needs CREATEDB), the mock AgentPulse on `E2E_MOCK_PORT` (9190), and the Taskify server on `E2E_API_PORT` (8091), with the app served by `vite preview` on `E2E_WEB_PORT` (4173) proxying `/api` to the server. The database and processes are removed when the run ends. Each test uses a tenant of its own.

## Sign-in

Until an identity provider is chosen (O2) the app takes a development token from `make dev-token` (`cmd/devtoken`). The token is kept in memory only, so a reload signs the user out.
