import { test, expect } from "../lib/stack";
import { step } from "../lib/evidence";

test("stack is up: server ready, admin UI connects with the management key", async ({ page, request, stack }) => {
  const ready = await request.get(`${stack.adminUrl}/ready`);
  expect(ready.status()).toBe(200);

  await page.goto(`${stack.uiUrl}/dashboard`);
  await expect(page.getByRole("heading", { name: "Dashboard", level: 1 })).toBeVisible();
  await step(page, "admin UI before key");

  await page.getByLabel("Management key").fill(stack.managementKey);
  await page.getByRole("button", { name: "Connect" }).click();
  await expect(page.getByText("Key in memory")).toBeVisible();
  await expect(page.getByRole("status", { name: "System health status" })).toBeVisible();
  await step(page, "admin dashboard connected");
});
