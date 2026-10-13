import { stopStack } from "./global-teardown";
import { oneportStackFile } from "./lib/stack";

export default async function globalTeardown() {
  stopStack(oneportStackFile);
}
