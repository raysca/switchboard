import { expect, it } from "vitest";
import { getBrowserCapabilities } from "./capabilities";

it("reports media capability without requesting permission", () => {
  let requested = false;
  const environment = {
    isSecureContext: true,
    navigator: {
      mediaDevices: {
        getUserMedia: () => {
          requested = true;
        },
      },
    },
    MediaRecorder: class {},
  };
  expect(getBrowserCapabilities(environment)).toEqual({
    secureContext: true,
    microphone: true,
    mediaRecorder: true,
  });
  expect(requested).toBe(false);
});
