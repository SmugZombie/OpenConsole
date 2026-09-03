/**
 * Working out which session a page is for, and with what credential.
 *
 * Kept apart from main.ts because this is the security-relevant parsing and it
 * should be testable without a DOM.
 */

export interface SessionLocator {
  sessionId: string;
  token: string;
}

/**
 * Reads the session and token out of a URL.
 *
 * The token lives in the fragment (`/s/<id>#<token>`) rather than the path or a
 * query parameter. A fragment is never transmitted in an HTTP request, so the
 * credential stays out of relay access logs, proxy logs and Referer headers,
 * while still surviving a copy-paste of the whole link. That is what makes this
 * a deliberate capability URL rather than a leaked secret.
 */
export function parseSessionURL(
  pathname: string,
  hash: string,
): SessionLocator | null {
  const m = /^\/s\/([^/]+)\/?$/.exec(pathname);
  if (!m || !m[1]) return null;
  const token = decodeURIComponent(hash.replace(/^#/, '')).trim();
  if (!token) return null;
  return { sessionId: decodeURIComponent(m[1]), token };
}

/** Splits a `session.token` ticket, the same format the CLI prints. */
export function parseTicket(raw: string): SessionLocator | null {
  const s = raw.trim();
  const dot = s.indexOf('.');
  if (dot <= 0 || dot === s.length - 1) return null;
  const sessionId = s.slice(0, dot);
  const token = s.slice(dot + 1);
  if (token.includes('.')) return null;
  return { sessionId, token };
}

/** Builds the page URL a guest opens, given a ticket's two halves. */
export function sessionURL(locator: SessionLocator): string {
  return `/s/${encodeURIComponent(locator.sessionId)}#${encodeURIComponent(locator.token)}`;
}
