// Main navigation, in order. `icon` is the SVG path (24x24 viewBox, stroked) drawn beside the name.
export const sections = [
  { path: "/dashboard", name: "Dashboard", icon: "M3 3h7v9H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 16h7v5H3z" },
  { path: "/agents", name: "Agents", icon: "M5 4h5v5H5zM14 15h5v5h-5zM7.5 9v3a3 3 0 0 0 3 3H14" },
  { path: "/providers", name: "Providers", icon: "M12 3l8 4.5v9L12 21l-8-4.5v-9z" },
  { path: "/mcp-servers", name: "MCP tools", icon: "M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18v3h3l6.3-6.3a4 4 0 0 0 5.4-5.4l-2.5 2.5-2.2-.5-.5-2.2z" },
  { path: "/http-tools", name: "HTTP tools", icon: "M10 13a5 5 0 0 0 7.5.5l3-3a5 5 0 0 0-7-7l-1.7 1.7M14 11a5 5 0 0 0-7.5-.5l-3 3a5 5 0 0 0 7 7l1.7-1.7" },
  { path: "/secrets", name: "Secrets", icon: "M7 11V8a5 5 0 0 1 10 0v3M5 11h14v10H5zM12 15v2" },
  { path: "/sessions", name: "Calls", icon: "M5 4h4l2 5-2.5 1.5a11 11 0 0 0 5 5L15 13l5 2v4a2 2 0 0 1-2 2A15 15 0 0 1 3 6a2 2 0 0 1 2-2z" },
  { path: "/server", name: "Server", icon: "M4 4h16v6H4zM4 14h16v6H4zM8 7h.01M8 17h.01" },
];
