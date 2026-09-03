import { describe, expect, it } from 'vitest';

import { parseSessionURL, parseTicket, sessionURL } from './locator';

describe('parseSessionURL', () => {
  it('reads the session from the path and the token from the fragment', () => {
    expect(parseSessionURL('/s/abc123', '#tok')).toEqual({
      sessionId: 'abc123',
      token: 'tok',
    });
  });

  it('tolerates a trailing slash', () => {
    expect(parseSessionURL('/s/abc123/', '#tok')?.sessionId).toBe('abc123');
  });

  it('returns null when the fragment was stripped', () => {
    // Chat clients truncate links at the #, and the result must fail loudly
    // rather than silently connecting with an empty token.
    expect(parseSessionURL('/s/abc123', '')).toBeNull();
    expect(parseSessionURL('/s/abc123', '#')).toBeNull();
    expect(parseSessionURL('/s/abc123', '#   ')).toBeNull();
  });

  it('ignores paths that are not session pages', () => {
    for (const p of ['/', '/s/', '/s/a/b', '/health', '/api/v1/tunnel']) {
      expect(parseSessionURL(p, '#tok'), p).toBeNull();
    }
  });
});

describe('sessionURL', () => {
  it('round-trips through parseSessionURL', () => {
    const locator = { sessionId: 'abc123', token: 'tok-en' };
    const url = sessionURL(locator);
    const [path, hash] = url.split('#');
    expect(parseSessionURL(path!, `#${hash}`)).toEqual(locator);
  });

  it('puts the token after the # so it is never sent to the server', () => {
    expect(sessionURL({ sessionId: 'a', token: 'secret' })).toBe('/s/a#secret');
  });
});

describe('parseTicket', () => {
  it('splits the format the CLI prints', () => {
    expect(parseTicket('abc.def')).toEqual({ sessionId: 'abc', token: 'def' });
  });

  it('tolerates whitespace from a chat client', () => {
    expect(parseTicket('  abc.def\n')).toEqual({ sessionId: 'abc', token: 'def' });
  });

  it('rejects malformed tickets', () => {
    for (const bad of ['', '   ', 'no-separator', '.', 'abc.', '.def', 'a.b.c']) {
      expect(parseTicket(bad), bad).toBeNull();
    }
  });
});
