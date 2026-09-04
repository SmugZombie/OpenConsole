import { KEY_SIZE, decodeBase32, encodeBase32, type KeyKind } from './crypto';

/**
 * Working out which session a page is for, and with what credential.
 *
 * Kept apart from main.ts because this is the security-relevant parsing and it
 * should be testable without a DOM.
 */

export interface SessionLocator {
  sessionId: string;
  token: string;
  /** The encryption key, absent when the session is not encrypted. */
  key?: Uint8Array;
  keyKind?: KeyKind;
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

  const fragment = decodeURIComponent(hash.replace(/^#/, '')).trim();
  if (!fragment) return null;

  const sessionId = decodeURIComponent(m[1]);
  const [token, keyField] = splitOnce(fragment);
  if (!token) return null;
  if (keyField === undefined) {
    return { sessionId, token };
  }
  const parsed = parseKeyField(keyField);
  if (!parsed) return null;
  return { sessionId, token, key: parsed.key, keyKind: parsed.kind };
}

/**
 * Splits a ticket, the same format the CLI prints:
 *
 *   <session>.<token>            no encryption
 *   <session>.<token>.k<key>     full access
 *   <session>.<token>.v<key>     watch only
 */
export function parseTicket(raw: string): SessionLocator | null {
  const fields = raw.trim().split('.');
  if (fields.length < 2 || fields.length > 3) return null;

  const [sessionId, token, keyField] = fields;
  if (!sessionId || !token) return null;
  if (keyField === undefined) {
    return { sessionId, token };
  }
  const parsed = parseKeyField(keyField);
  if (!parsed) return null;
  return { sessionId, token, key: parsed.key, keyKind: parsed.kind };
}

/**
 * Reads the letter and the encoded key.
 *
 * The letter is in the ticket so a client knows what it holds without asking
 * the relay, which must not be able to talk a guest into misusing its own key.
 */
function parseKeyField(field: string): { key: Uint8Array; kind: KeyKind } | null {
  const kind = field[0] === 'k' ? 'root' : field[0] === 'v' ? 'viewer' : null;
  if (!kind) return null;
  try {
    const key = decodeBase32(field.slice(1));
    if (key.length !== KEY_SIZE) return null;
    return { key, kind };
  } catch {
    return null;
  }
}

/** Splits on the first '.', returning the tail only when there is one. */
function splitOnce(s: string): [string, string | undefined] {
  const i = s.indexOf('.');
  if (i < 0) return [s, undefined];
  return [s.slice(0, i), s.slice(i + 1)];
}

/** Builds the page URL a guest opens, preserving any key. */
export function sessionURL(locator: SessionLocator): string {
  const fragment =
    locator.key && locator.keyKind
      ? `${locator.token}.${locator.keyKind === 'root' ? 'k' : 'v'}${encodeBase32(locator.key)}`
      : locator.token;
  return `/s/${encodeURIComponent(locator.sessionId)}#${fragment}`;
}
