import { describe, expect, it } from "vitest";
import { emptyForm, fromForm, toForm } from "./httpToolForm";

describe("httpToolForm", () => {
  it("round-trips a definition through the text fields", () => {
    const def = {
      name: "get_order",
      description: "Look up an order.",
      method: "GET",
      url: "https://api.example.com/orders/{order_id}",
      headers: { "X-Tenant": "acme" },
      auth: { secret: "crm-api", header: "X-Api-Key" },
      input_schema: { type: "object", properties: { order_id: { type: "string" } }, required: ["order_id"] },
      timeout_ms: 5000,
      max_response_bytes: 4096,
      response_pick: ["/data/status"],
    };
    expect(fromForm(toForm(def))).toEqual({ definition: def });
  });

  it("omits what is empty and keeps Authorization as the default header", () => {
    const form = { ...emptyForm(), name: "n", description: "d", url: "https://a.example/x", authSecret: "crm-api" };
    const result = fromForm(form);
    expect(result).toEqual({ definition: expect.objectContaining({ headers: undefined, auth: { secret: "crm-api", header: undefined }, timeout_ms: undefined, response_pick: undefined }) });
    expect(toForm({ name: "n", description: "d", method: "GET", url: "u", input_schema: {}, auth: { secret: "s", header: "Authorization" } }).authHeader).toBe("");
  });

  it("reports the first problem in plain words", () => {
    expect(fromForm({ ...emptyForm(), schema: "{" })).toEqual({ error: "Input schema is not valid JSON." });
    expect(fromForm({ ...emptyForm(), headers: "nonsense" })).toEqual({ error: 'Header "nonsense" needs the form Name: value.' });
    expect(fromForm({ ...emptyForm(), timeoutMs: "fast" })).toEqual({ error: "Timeout must be a whole number." });
    expect(fromForm({ ...emptyForm(), maxResponseBytes: "-5" })).toEqual({ error: "Response limit must be a whole number." });
  });
});
