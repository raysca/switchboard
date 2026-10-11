import { stopStack } from "./global-teardown";
import { graphStackFile } from "./lib/stack";

export default async function globalTeardown() {
  stopStack(graphStackFile);
}
