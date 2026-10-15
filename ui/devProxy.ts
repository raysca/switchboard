/**
 * The Authorization value the dev server's proxy adds to a management API request, or undefined to add
 * nothing. It signs in only requests the dashboard itself marked (X-Switchboard-UI: 1): a custom header
 * a web page on another origin cannot send without a CORS preflight, which the dev server refuses. So
 * opening some other site in the same browser cannot use the proxy to act with the management key.
 */
export function proxyAuthorization(
  headers: Record<string, string | string[] | undefined>,
  key: string | undefined,
): string | undefined {
  if (!key || headers["x-switchboard-ui"] !== "1") return undefined;
  return `Bearer ${key}`;
}
