import createClient from "openapi-fetch";
import type { components, paths } from "./schema";

export type Sandbox = components["schemas"]["Sandbox"];
export type Principal = components["schemas"]["Principal"];

// The session is an HttpOnly cookie; X-Dawnbx: 1 is the server's CSRF check on cookie-authed writes.
export const api = createClient<paths>({ headers: { "X-Dawnbx": "1" } });

let onSignedOut = () => {};
export const setSignedOut = (fn: () => void) => (onSignedOut = fn);

api.use({
  onResponse({ request, response }) {
    if (response.status === 401 && !request.url.endsWith("/v1/login")) onSignedOut();
  },
});

// must unwraps an openapi-fetch result, throwing the server's "message (hint)".
export async function must<T>(
  p: Promise<{ data?: T; error?: unknown; response: Response }>,
): Promise<T> {
  const { data, error, response } = await p;
  if (response.status === 401 && !response.url.endsWith("/v1/login"))
    throw new Error("signed out (session expired); sign in again");
  if (!response.ok) {
    const e = error as components["schemas"]["Error"] | undefined;
    throw new Error(
      e?.message
        ? e.message + (e.hint ? ` (${e.hint})` : "")
        : `${response.status} ${response.statusText}`,
    );
  }
  return data as T;
}

// Single-quotes s for sh -c.
export const q = (s: string) => `'${s.replace(/'/g, `'\\''`)}'`;

function dur(ms: number) {
  const s = Math.round(Math.abs(ms) / 1000);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  return `${Math.floor(s / 86400)}d`;
}
export const age = (t: string) => dur(Date.now() - Date.parse(t));
export const left = (t?: string | null) => (t ? dur(Date.parse(t) - Date.now()) : "never");
export const when = (t?: string | null) => (t ? new Date(t).toLocaleString() : "–");
