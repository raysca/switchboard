import http from "node:http";
import { test, expect } from "../lib/oneport-stack";
import { api, botReply, connectAdmin, openFlowTest, sendText, startCall, stage } from "../lib/flows-helpers";

// The one-port server: the embedded UI, the management API and the voice socket all on the
// public origin, with the browser admitted only by origin_allow_same_host. Serial: the last
// test uses up this client address's failed-key allowance.
test.describe.configure({ mode: "serial" });

test("the UI, its API calls and the voice socket all work from the one public origin", async ({ page, stack }) => {
  const cspViolations: string[] = [];
  page.on("console", (m) => {
    if (/Content Security Policy|Refused to/.test(m.text())) cspViolations.push(m.text());
  });

  await page.goto(stack.publicUrl); // the embedded UI, not Vite
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await page.goto(`${stack.publicUrl}/flows`); // a client route survives a hard load (SPA fallback)
  await expect(page).toHaveURL(/\/flows$/);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();

  await api(stack, "/flows", {
    method: "POST",
    body: JSON.stringify({ name: "E2E One Port", config: { stt: stage({ model: "chirp" }), llm: stage({ model: "fake-llm", system_prompt: "You are ONEPORT. Answer in one short sentence." }), tts: stage() } }),
  });
  await connectAdmin(page, stack); // the key goes to the same-origin API
  await openFlowTest(page, "E2E One Port");
  await startCall(page); // the socket is same-origin, admitted only by origin_allow_same_host
  await sendText(page, "hello");
  await expect(botReply(page, /./)).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("disconnect-btn").click();
  expect(cspViolations).toEqual([]);
});

test("the management and operational surfaces are as exposed as intended", async ({ request, stack }) => {
  expect((await request.get(`${stack.publicUrl}/health`)).status()).toBe(200);
  for (const p of ["/metrics", "/ready", "/debug/timeline"]) expect((await request.get(`${stack.publicUrl}${p}`)).status(), p).not.toBe(200);
  const noKey = await request.get(`${stack.publicUrl}/api/v1/server-config`);
  expect(noKey.status()).toBe(401);
  const withKey = await request.get(`${stack.publicUrl}/api/v1/server-config`, { headers: { Authorization: `Bearer ${stack.managementKey}` } });
  expect(withKey.status()).toBe(200);
  expect(withKey.headers()["cache-control"]).toBe("no-store");
  expect(((await withKey.json()) as { data: { client_ip: string } }).data.client_ip).toMatch(/^(127\.0\.0\.1|::1)$/);
  expect((await request.get(`${stack.publicUrl}/assets/does-not-exist.js`)).status()).toBe(404);
});

test("repeated wrong keys are throttled and the page says so", async ({ page, request, stack }) => {
  const wrong = { headers: { Authorization: "Bearer wrong" } };
  const url = `${stack.publicUrl}/api/v1/server-config`;
  // Earlier tests already spent some of the allowance (the missing key counts), so: 401s, then 429 within 11 tries.
  const seen: number[] = [];
  for (let i = 0; i < 11 && seen.at(-1) !== 429; i++) seen.push((await request.get(url, wrong)).status());
  expect(seen.at(-1), `statuses ${seen}`).toBe(429);
  expect(seen.slice(0, -1).every((s) => s === 401)).toBe(true);
  await page.goto(stack.publicUrl);
  const key = page.getByLabel("Management key");
  const connect = page.getByRole("button", { name: "Connect", exact: true });
  // (a) a WRONG key while the allowance is spent shows the server's message, not an empty screen or a raw error.
  await key.fill("wrong");
  await connect.click();
  await expect(page.getByText(/too many failed attempts; try again shortly/i).first()).toBeVisible();
  // (b) the CORRECT key is never throttled, so the admin still gets in.
  await key.fill(stack.managementKey);
  await connect.click();
  // The dashboard's "System <health> · up <time>" status renders only from an authenticated
  // /api/v1/operations/status response (the key badge shows for any non-empty key), and a refused key
  // would show the rejection message instead.
  await expect(page.getByText(/^System \w+ · up /)).toBeVisible();
  await expect(page.getByText(/Management key was rejected/)).toHaveCount(0);
  await expect(page.getByText(/too many failed attempts/i)).toHaveCount(0);
});

test("a foreign browser origin is refused the voice socket", async ({ stack }) => {
  // Negative control: the same upgrade with an Origin that is not this host must not be admitted.
  const status = await new Promise<number>((resolve, reject) => {
    const req = http.request(`${stack.publicUrl}/ws`, {
      headers: { Connection: "Upgrade", Upgrade: "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==", Origin: "https://evil.example" },
    });
    req.on("upgrade", (res) => (res.socket.destroy(), resolve(res.statusCode ?? 0)));
    req.on("response", (res) => (res.resume(), resolve(res.statusCode ?? 0)));
    req.on("error", reject);
    req.end();
  });
  expect(status).toBe(403);
});
