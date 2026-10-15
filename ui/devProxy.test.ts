import { describe, expect, it } from "vitest";
import { proxyAuthorization } from "./devProxy";

describe("dev proxy credentials", () => {
  it("adds the management key only to requests the dashboard marked as its own", () => {
    expect(proxyAuthorization({ "x-switchboard-ui": "1" }, "secret")).toBe("Bearer secret");
  });

  it("adds nothing to a request without the marker, so a cross-site page cannot borrow the key", () => {
    expect(proxyAuthorization({}, "secret")).toBeUndefined();
    expect(proxyAuthorization({ "x-switchboard-ui": "0" }, "secret")).toBeUndefined();
    expect(proxyAuthorization({ authorization: "Bearer other" }, "secret")).toBeUndefined();
  });

  it("adds nothing when the dev server has no key", () => {
    expect(proxyAuthorization({ "x-switchboard-ui": "1" }, undefined)).toBeUndefined();
    expect(proxyAuthorization({ "x-switchboard-ui": "1" }, "")).toBeUndefined();
  });
});
