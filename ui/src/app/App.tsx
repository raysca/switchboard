import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  BrowserRouter,
  MemoryRouter,
  Navigate,
  NavLink,
  Route,
  Routes,
  useLocation,
  useParams,
} from "react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError, createApiClient } from "../api/client";
import {
  ActionButton,
  ErrorState,
  FormField,
  Status,
} from "../components/primitives";
import { getBrowserCapabilities } from "./capabilities";
import { OnboardingView } from "../routes/onboarding/OnboardingView";
import { ProvidersView } from "../routes/providers/ProvidersView";
import { FlowsView } from "../routes/agents/FlowsView";
import { FlowPage } from "../routes/agents/FlowPage";
import { MCPView } from "../routes/mcp/MCPView";
import { CallsView } from "../routes/sessions/CallsView";
import { CallView } from "../routes/sessions/CallView";
import { DashboardView } from "../routes/dashboard/DashboardView";
import { ServerView } from "../routes/server/ServerView";
import { sections } from "../routes/sections";

function RouteFocus() {
  const location = useLocation();
  const previousPath = useRef(location.pathname);
  useEffect(() => {
    if (previousPath.current !== location.pathname) {
      previousPath.current = location.pathname;
      document.querySelector<HTMLElement>("main h1")?.focus();
    }
  }, [location.pathname]);
  return null;
}

/** An old /agents/:id bookmark opens the same flow. */
function LegacyAgentRedirect() {
  const { agentId = "" } = useParams();
  return <Navigate to={`/flows/${encodeURIComponent(agentId)}`} replace />;
}

export function App({ initialPath }: { initialPath?: string }) {
  const [keyValue, setKeyValue] = useState<string | null>(null);
  const [draft, setDraft] = useState("");
  const [authError, setAuthError] = useState<ApiError | null>(null);
  const auth = useRef({ key: null as string | null, generation: 0 });
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
        <RouteFocus />
        <a className="skip-link" href="#main">
          Skip to content
        </a>
        <div className="app-layout">
          <aside className="sidebar">
            <div className="brand">
              <span className="brand-mark" aria-hidden="true">
                ⌁
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
                  <span className="nav-icon" aria-hidden="true">
                    {section.icon}
                  </span>
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
                  {keyValue ? "Key in memory" : "No management key"}
                </Status>
                {keyValue && (
                  <ActionButton type="button" className="action-button-small" onClick={clearKey}>
                    Clear key
                  </ActionButton>
                )}
              </div>
            </div>
            <main id="main" tabIndex={-1}>
              {!keyValue && (
                <section className="key-bar" aria-labelledby="access-title">
                  <div>
                    <h2 id="access-title">Management access</h2>
                    <p>The key stays in memory only. Reloading clears it.</p>
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
                <Route path="/flows" element={<FlowsView client={client} keyValue={keyValue} />} />
                <Route path="/flows/new" element={<FlowPage client={client} keyValue={keyValue} />} />
                <Route path="/flows/:flowId" element={<FlowPage client={client} keyValue={keyValue} />} />
                <Route path="/agents" element={<Navigate to="/flows" replace />} />
                <Route path="/agents/:agentId" element={<LegacyAgentRedirect />} />
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
