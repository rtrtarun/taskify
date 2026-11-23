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
