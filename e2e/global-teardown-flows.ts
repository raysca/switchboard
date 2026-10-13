import { stopStack } from "./global-teardown";
import { flowsStackFile } from "./lib/stack";

export default async function globalTeardown() {
  stopStack(flowsStackFile);
}
