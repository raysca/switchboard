import { stopStack } from "./global-teardown";
import { fakeStackFile } from "./lib/stack";

export default async function globalTeardown() {
  stopStack(fakeStackFile);
}
