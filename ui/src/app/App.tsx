import { useEffect, useRef, useState, type FormEvent } from "react";
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
import { ApiError, createApiClient } from "../api/client";
import {
  ActionButton,
  ErrorState,
  FormField,
  Status,
} from "../components/primitives";
import { getBrowserCapabilities } from "./capabilities";
import { SectionPage, sections } from "../routes/sections";

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
              <span>go-voice</span>
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
                  {section.name}
                </NavLink>
              ))}
            </nav>
            <div className="sidebar-foot">Voice operations</div>
          </aside>
          <div className="workspace">
            <div className="topbar">
              <span>Operations</span>
              <Status tone={keyValue ? "good" : "neutral"}>
                {keyValue ? "Key in memory" : "No management key"}
              </Status>
            </div>
            <main id="main" tabIndex={-1}>
              <Routes>
                <Route
                  path="/"
                  element={<Navigate to="/dashboard" replace />}
                />
                {sections.map((section) => (
                  <Route
                    key={section.path}
                    path={section.path}
                    element={
                      <SectionPage
                        section={section}
                        keyValue={keyValue}
                        readConfig={client.serverConfig}
                      />
                    }
                  />
                ))}
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
              <section className="access-panel" aria-labelledby="access-title">
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
                {keyValue ? (
                  <ActionButton onClick={clearKey}>Clear key</ActionButton>
                ) : (
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
                )}
              </section>
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
