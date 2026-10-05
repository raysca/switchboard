# Task 18 — Versioned AgentMCPTool associations and safe lifecycle

## Delivered

- Agent create and MCP-tool replacement now publish immutable, version-scoped `(server ID, remote tool name, selected schema hash, required)` associations. Config-only version publication preserves existing selections; duplicate copies them into its new version.
- Agent readiness compares every current selection with the latest discovery and distinguishes `needs_tool_review` (schema drift/tool disappearance), `tool_dependency_unavailable` (required server disabled/unavailable), and `ready_with_tool_warning` (optional dependency unavailable). A required unavailable selection blocks new-session resolution; optional tools are omitted from a session's runtime set.
- `server/runtime.StartSession` captures usable tools before persisting the session and maps them to `core.ToolDefinition` and `core.ToolExecutor`. MCP SDK, credentials, and MCP types do not enter core. Required capture failure rejects session creation; optional capture failure skips that wrapper.
- Task17 MCP execution now exposes an in-memory captured handle pinning the endpoint, selected schema, and resolved bearer credential. Both validation-console execution and runtime wrappers use the same argument-validation, deadline, redaction, safe-error, and trace path. Later disable/discovery refresh does not change an already captured handle.
- Session storage records safe runtime snapshot metadata and exact active wrapper references in `session_mcp_tools`. No plaintext credentials are persisted. Session close prevents new executions, waits for in-flight tool calls, ends the session, and finalizes pending deletes.
- MCP server lifecycle now supports reversible disable/enable, normal delete with current-agent/active-session conflict checks and dependency details, and explicitly confirmed detach-and-delete. Detach transactionally publishes new versions for current agents without that server's selections, disables the server, and drains active sessions before removing only the live server row. Historical versions, discoveries, session snapshots, and tool-call evidence remain intact.
- Added SQLite/PostgreSQL migration `004_mcp_lifecycle.sql`, authenticated management routes, and API/security/contract docs.

## TDD and verification

The initial focused storage test failed because `DeleteServer`, `DisableServer`, `DetachAndDelete`, and `SessionRepository.End` were absent. The agent readiness test also failed because versioned selection/readiness/snapshot APIs were absent. After implementation, focused suites passed.

Passed with the project SDK override:

```text
SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk
CGO_LDFLAGS=-Wl,-w
go test -race -count=10 ./runtime ./mcp ./agent ./api ./storage -run 'TestSessionUsesCapturedMCPWrapperAfterDisableAndRefresh|TestMCPDisableDeleteConflictAndConfirmedDetachRoutes|TestAgentMCPToolReplacementPublishesVersionedAssociation|TestMCPSelectionsAreVersionScopedAndSchemaDriftRequiresExplicitReview|TestOptionalUnavailableMCPToolWarnsWithoutBlockingVoiceReadiness|TestMCPDisableAndDeleteRespectCurrentAndActiveVersionReferences|TestConcurrentSessionCloseAndMCPDeleteAlwaysDrainsSafely'
go test -race -count=1 ./...
go vet ./...
go build ./...
git diff --check
```

The runtime test changes discovery schema and disables the server after session capture, executes through the old captured schema, verifies deletion remains pending during both active session and in-flight tool call, drains the session, and confirms historical schema/hash remain readable. The concurrent close/delete test runs ten detach/drain races per test execution. All focused tests ran ten times under the race detector.

## Assumptions and remaining verification

- Task17's current remote Streamable HTTP-only transport and server-side Bearer credential resolver remain the supported MCP execution boundary; process/stdio transport stays rejected.
- For uninterrupted active-session execution, the wrapper keeps the resolved Bearer credential in process memory until session drain. Rotating/deleting the credential affects new captures, not existing ones. Immediate revocation requires ending the active session or revoking upstream. The token is never serialized or exposed on a public field.
- Runtime capture rejects required schemas exceeding core's current 32 KiB tool-definition limit. Optional oversized schemas are skipped. Tool-name collisions are avoided with a deterministic server/name hash suffix.
- PostgreSQL SQL paths and migration were implemented dialect-compatibly, but `TEST_POSTGRES_DSN` was not available for live PostgreSQL execution. SQLite migration upgrade and lifecycle tests passed.
- Independent code review remains pending at the parent orchestration gate.
