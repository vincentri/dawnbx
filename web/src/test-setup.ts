// @ts-expect-error -- tsconfig's `types` allowlist is just vite/client, so the
// node:buffer types are not loaded; the runtime import is fine.
import { File as NodeFile } from "node:buffer";
import "@/test-fetch"; // installs the fetch stub before any page module loads
import "@testing-library/jest-dom/vitest";

// Loaded by every test file (jsdom or node). Only touches globals that jsdom
// is missing and Radix/user-event reach for; harmless under `environment: node`.
if (typeof globalThis.window !== "undefined") {
  if (!Element.prototype.hasPointerCapture) {
    Element.prototype.hasPointerCapture = () => false;
    Element.prototype.setPointerCapture = () => {};
    Element.prototype.releasePointerCapture = () => {};
  }
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {};
  if (!globalThis.ResizeObserver) {
    globalThis.ResizeObserver = class {
      observe() {}
      unobserve() {}
      disconnect() {}
    } as unknown as typeof ResizeObserver;
  }
  // jsdom has no PointerEvent; Radix's Select/Menu open on pointerdown.
  if (!globalThis.PointerEvent)
    globalThis.PointerEvent = MouseEvent as unknown as typeof PointerEvent;
  if (!globalThis.matchMedia) {
    globalThis.matchMedia = ((q: string) => ({
      matches: false,
      media: q,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    })) as unknown as typeof matchMedia;
  }

  // The dashboard is served from /ui/ and calls the API with root-relative
  // URLs ("/v1/me"). A browser resolves those against the document; undici's
  // Request, which is what runs under jsdom, has no base and throws. Anchor
  // them so the code under test issues exactly the requests it issues in a
  // browser.
  const NativeRequest = globalThis.Request;
  class AnchoredRequest extends NativeRequest {
    constructor(input: RequestInfo | URL, init?: RequestInit) {
      super(
        typeof input === "string" && input.startsWith("/")
          ? new URL(input, window.location.origin).href
          : input,
        init,
      );
    }
  }
  globalThis.Request = AnchoredRequest as unknown as typeof Request;

  // undici's Request — the one openapi-fetch hands to fetch — only accepts its
  // own Blob/File, which jsdom does not provide. A File upload therefore dies
  // before the request is made. Hand tests the platform File instead.
  globalThis.File = NodeFile as unknown as typeof globalThis.File;
}
