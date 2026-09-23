const API_BASE_URL = import.meta.env.VITE_API_BASE_URL ??
    (import.meta.env.DEV ? 'http://localhost:8001' : '');

const WS_BASE_URL = import.meta.env.VITE_WS_URL ??
    (import.meta.env.DEV ? 'ws://localhost:8001'
     : `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}`);

// AGENTEVALS-GO FORK: path prefix this app is served under behind a reverse
// proxy (e.g. "/evals-go"), baked in at build time via VITE_API_BASE_URL -
// the same value API_BASE_URL uses in production. Resolved separately from
// API_BASE_URL because in DEV mode API_BASE_URL is a full origin
// ("http://localhost:8001"), not a path, so reusing it directly would
// double up the host. Not present upstream - Python's UI has no WS
// transport for its live feed; see uiUpdatesWs below and
// LiveStreamingView.tsx's connectWS. Re-add both on the next UI re-sync
// from agentevals/ui.
const BASE_PATH = import.meta.env.DEV ? '' : (import.meta.env.VITE_API_BASE_URL ?? '');

export const config = {
  api: {
    baseUrl: API_BASE_URL,
    endpoints: {
      health: `${API_BASE_URL}/api/health`,
      metrics: `${API_BASE_URL}/api/metrics`,
      evaluate: `${API_BASE_URL}/api/evaluate`,
      evaluateStream: `${API_BASE_URL}/api/evaluate/stream`,
      runs: `${API_BASE_URL}/api/runs`,
      evalSets: `${API_BASE_URL}/api/evalsets`,
      validateEvalSet: `${API_BASE_URL}/api/validate/eval-set`,
      streamingCreateEvalSet: `${API_BASE_URL}/api/streaming/create-eval-set`,
      streamingGetTrace: `${API_BASE_URL}/api/streaming/get-trace`,
      streamingSessions: `${API_BASE_URL}/api/streaming/sessions`,
      uiUpdatesStream: `${API_BASE_URL}/stream/ui-updates`,
      debugBundle: `${API_BASE_URL}/api/debug/bundle`,
      debugLoad: `${API_BASE_URL}/api/debug/load`,
      authMe: `${API_BASE_URL}/auth/me`,
      authLogin: `${API_BASE_URL}/auth/login`,
      authLogout: `${API_BASE_URL}/auth/logout`,
    },
  },
  websocket: {
    tracesUrl: `${WS_BASE_URL}/ws/traces`,
    // AGENTEVALS-GO FORK: see BASE_PATH's comment above.
    uiUpdatesWs: `${WS_BASE_URL}${BASE_PATH}/ws/ui-updates`,
  },
} as const;
