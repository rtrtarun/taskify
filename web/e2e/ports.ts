export const API_PORT = Number(process.env.E2E_API_PORT ?? 8091);
export const MOCK_PORT = Number(process.env.E2E_MOCK_PORT ?? 9190);
export const LLM_PORT = Number(process.env.E2E_LLM_PORT ?? 9191);
export const WEB_PORT = Number(process.env.E2E_WEB_PORT ?? 4173);
export const JWT_SECRET = "e2e-jwt-secret";
export const API_URL = `http://127.0.0.1:${API_PORT}`;
export const LLM_URL = `http://127.0.0.1:${LLM_PORT}`;
export const MOCK_URL = `http://127.0.0.1:${MOCK_PORT}`;
