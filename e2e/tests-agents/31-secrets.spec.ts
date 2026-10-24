import { test, expect } from "../lib/agents-stack";
import { step } from "../lib/evidence";
import { connectAdmin } from "../lib/agents-helpers";

// The Secrets page against the real server: add an encrypted secret, see it listed without its value, and delete it.
test("secrets page: add a stored secret, never see its value, delete it", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Secrets", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "Secrets", level: 1 })).toBeVisible();
  // the environment secret the stack starts with is listed, read-only
  const table = page.getByRole("table", { name: "Secrets available to tools and MCP servers" });
  await expect(table.getByText("MCP_TOKEN_TEST")).toBeVisible();

  await page.getByRole("button", { name: "Add secret" }).click();
  const dialog = page.getByRole("dialog", { name: "Add secret" });
  await dialog.getByLabel("Name").fill("e2e-crm");
  await dialog.getByLabel("Value").fill("e2e-secret-value-xyz");
  await dialog.getByRole("button", { name: "Save" }).click();
  await expect(dialog).toHaveCount(0);
  const row = table.getByRole("row", { name: /e2e-crm/ });
  await expect(row).toContainText("Encrypted store");
  await expect(page.getByText("e2e-secret-value-xyz")).toHaveCount(0);
  await step(page, "secrets page");

  await row.getByRole("button", { name: "Delete" }).click();
  await page.getByRole("button", { name: "Delete secret" }).click();
  await expect(table.getByText("e2e-crm")).toHaveCount(0);
});
