import { stopStack } from "./global-teardown";
import { agentsStackFile } from "./lib/stack";

export default async function globalTeardown() {
  stopStack(agentsStackFile);
}
