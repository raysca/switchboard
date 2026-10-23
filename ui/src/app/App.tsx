import { lazy, Suspense, useEffect, useRef, useState, type FormEvent } from "react";
import {
  BrowserRouter,
  MemoryRouter,
  Navigate,
  NavLink,
  Route,
  Routes,
  useLocation,
} from "react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError, createApiClient, PROXIED_ACCESS, probeProxiedAccess } from "../api/client";
import {
  ActionButton,
  ErrorState,
  FormField,
  LoadingState,
  Status,
} from "../components/primitives";
import { getBrowserCapabilities } from "./capabilities";
import { ProvidersView } from "../routes/providers/ProvidersView";
import { AgentsView } from "../routes/agents/AgentsView";
import { MCPView } from "../routes/mcp/MCPView";
import { CallsView } from "../routes/sessions/CallsView";
import { DashboardView } from "../routes/dashboard/DashboardView";
import { ServerView } from "../routes/server/ServerView";
import { sections } from "../routes/sections";

// The agent editor (React Flow), the call detail and first run are only opened sometimes: load them on demand so the first screen stays small.
const OnboardingView = lazy(() => import("../routes/onboarding/OnboardingView").then((m) => ({ default: m.OnboardingView })));
const AgentPage = lazy(() => import("../routes/agents/AgentPage").then((m) => ({ default: m.AgentPage })));
const CallView = lazy(() => import("../routes/sessions/CallView").then((m) => ({ default: m.CallView })));

function HandoffFocus() {
  const location = useLocation();
  const previousPath = useRef(location.pathname);
  useEffect(() => {
    if (previousPath.current === location.pathname) return;
    previousPath.current = location.pathname;
    const heading = () => document.querySelector<HTMLElement>("main h1");
    if (heading()) {
      heading()?.focus();
      return;
    }
    // A lazy route is still loading: focus its heading as soon as it mounts (give up after a few seconds).
    const main = document.querySelector("main");
    if (!main) return;
    const observer = new MutationObserver(() => {
      const h1 = heading();
      if (!h1) return;
      h1.focus();
      observer.disconnect();
    });
    observer.observe(main, { childList: true, subtree: true });
    const giveUp = window.setTimeout(() => observer.disconnect(), 5000);
    return () => {
      observer.disconnect();
      window.clearTimeout(giveUp);
    };
  }, [location.pathname]);
  return null;
}

/** A magic link carries the key in the address fragment (#key=...): never sent to a server, never logged. */
function keyFromAddress(): string | null {
  return new URLSearchParams(window.location.hash.replace(/^#/, "")).get("key")?.trim() || null;
}

export function App({ initialPath }: { initialPath?: string }) {
  const [linkKey] = useState(keyFromAddress);
  const [keyValue, setKeyValue] = useState<string | null>(linkKey);
  const [draft, setDraft] = useState("");
  const [authError, setAuthError] = useState<ApiError | null>(null);
  const auth = useRef({ key: linkKey, generation: 0 });
  const keyInput = useRef<HTMLInputElement>(null);
  const [queryClient] = useState(
    () => new QueryClient({ defaultOptions: { queries: { retry: false } } }),
  );
  const client = createApiClient(
    () => auth.current,
    fetch,
    (error, generation) => {
      if (generation !== auth.current.generation) return;
      auth.current = { key: null, generation: generation + 1 };
      setAuthError(
        new ApiError(
          "Management key was rejected. Enter a key to reconnect.",
          error.code,
          error.status,
          error.requestId,
        ),
      );
      setKeyValue(null);
      queryClient.clear();
    },
  );
  const Router = initialPath ? MemoryRouter : BrowserRouter;
  const capabilities = getBrowserCapabilities();

  useEffect(() => {
    if (authError) keyInput.current?.focus();
  }, [authError]);

  // A magic link's key is used from memory and removed from the address bar (and the history entry) at once.
  useEffect(() => {
    if (linkKey) window.history.replaceState(window.history.state, "", window.location.pathname + window.location.search);
  }, [linkKey]);

  // Without a link, ask the dev server whether its proxy signs the dashboard in. Only the dev server has
  // such a proxy: against a hosted server a keyless request is a failed login, counted and rate limited.
  useEffect(() => {
    if (linkKey || !import.meta.env.DEV) return;
    let cancelled = false;
    void probeProxiedAccess().then((signedIn) => {
      if (cancelled || !signedIn || auth.current.key) return; // a key pasted meanwhile wins
      auth.current = { key: PROXIED_ACCESS, generation: auth.current.generation + 1 };
      queryClient.clear();
      setKeyValue(PROXIED_ACCESS);
    });
    return () => {
      cancelled = true;
    };
  }, [linkKey, queryClient]);

  function clearKey() {
    auth.current = { key: null, generation: auth.current.generation + 1 };
    setKeyValue(null);
    setAuthError(null);
    queryClient.clear();
  }

  function connect(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const value = draft.trim();
    if (!value) return;
    auth.current = { key: value, generation: auth.current.generation + 1 };
    setAuthError(null);
    queryClient.clear();
    setKeyValue(value);
    setDraft("");
  }

  return (
    <QueryClientProvider client={queryClient}>
      <Router {...(initialPath ? { initialEntries: [initialPath] } : {})}>
        <HandoffFocus />
        <a className="skip-link" href="#main">
          Skip to content
        </a>
        <div className="app-layout">
          <aside className="sidebar">
            <div className="brand">
              <span className="brand-mark" aria-hidden="true">
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
                  <path d="M4 12h3l2-6 4 12 2-6h5" />
                </svg>
              </span>
              <span>Switchboard</span>
            </div>
            <nav aria-label="Main navigation" className="navigation">
              {sections.map((section) => (
                <NavLink
                  key={section.path}
                  to={section.path}
                  className={({ isActive }) =>
                    isActive ? "nav-link nav-link--current" : "nav-link"
                  }
                >
                  <svg className="nav-icon" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                    <path d={section.icon} />
                  </svg>
                  <span className="nav-text">{section.name}</span>
                </NavLink>
              ))}
            </nav>
            <div className="sidebar-foot">Voice operations</div>
          </aside>
          <div className="workspace">
            <div className="topbar">
              <span>Operations</span>
              <div className="topbar-key">
                <Status tone={keyValue ? "good" : "neutral"}>
                  {keyValue === PROXIED_ACCESS ? "Signed in by the server" : keyValue ? "Key in memory" : "No management key"}
                </Status>
                {keyValue && keyValue !== PROXIED_ACCESS && (
                  <ActionButton type="button" onClick={clearKey}>Sign out</ActionButton>
                )}
              </div>
            </div>
            <main id="main" tabIndex={-1}>
              {!keyValue && (
                <section className="key-bar" aria-labelledby="access-title">
                  <div>
                    <h2 id="access-title">Management access</h2>
                    <p>
                      The key stays in this tab only, until you sign out or reload. It is sent as a bearer token to <code>/api/v1</code>.
                    </p>
                    {authError && (
                      <ErrorState
                        title="Management access rejected"
                        error={authError}
                      />
                    )}
                  </div>
                  <form onSubmit={connect}>
                    <FormField
                      id="management-key"
                      label="Management key"
                      inputRef={keyInput}
                      type="password"
                      autoComplete="off"
                      value={draft}
                      onChange={(event) => setDraft(event.target.value)}
                    />
                    <ActionButton type="submit">Connect</ActionButton>
                  </form>
                </section>
              )}
              <Suspense fallback={<LoadingState label="Loading" />}>
              <Routes>
                <Route
                  path="/"
                  element={<Navigate to="/dashboard" replace />}
                />
                <Route
                  path="/onboarding"
                  element={
                    <OnboardingView client={client} keyValue={keyValue} />
                  }
                />
                <Route
                  path="/providers"
                  element={
                    <ProvidersView client={client} keyValue={keyValue} />
                  }
                />
                <Route path="/agents" element={<AgentsView client={client} keyValue={keyValue} />} />
                <Route path="/agents/new" element={<AgentPage client={client} keyValue={keyValue} />} />
                <Route path="/agents/:agentId" element={<AgentPage client={client} keyValue={keyValue} />} />
                <Route
                  path="/mcp-servers"
                  element={<MCPView client={client} keyValue={keyValue} />}
                />
                <Route
                  path="/mcp-servers/:serverId"
                  element={<MCPView client={client} keyValue={keyValue} />}
                />
                <Route
                  path="/sessions"
                  element={<CallsView client={client} keyValue={keyValue} />}
                />
                <Route
                  path="/sessions/:sessionId"
                  element={
                    <CallView client={client} keyValue={keyValue} />
                  }
                />
                <Route
                  path="/dashboard"
                  element={
                    <DashboardView client={client} keyValue={keyValue} />
                  }
                />
                <Route
                  path="/server"
                  element={
                    <ServerView client={client} keyValue={keyValue} />
                  }
                />
                <Route
                  path="*"
                  element={
                    <div className="section-heading">
                      <h1 tabIndex={-1}>Area not found</h1>
                      <p>Choose an area from the navigation.</p>
                    </div>
                  }
                />
              </Routes>
              </Suspense>
              <section
                className="capability-line"
                aria-label="Browser capabilities"
              >
                <span>Browser check</span>
                <span>
                  Secure context:{" "}
                  {capabilities.secureContext ? "available" : "unavailable"}
                </span>
                <span>
                  Microphone API:{" "}
                  {capabilities.microphone ? "available" : "unavailable"}
                </span>
                <span>
                  Media recording:{" "}
                  {capabilities.mediaRecorder ? "available" : "unavailable"}
                </span>
              </section>
            </main>
          </div>
        </div>
      </Router>
    </QueryClientProvider>
  );
}
