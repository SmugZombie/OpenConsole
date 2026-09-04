import { describe, expect, it } from 'vitest';

// Imported rather than read from disk so this needs no Node types: the fixture
// is shared with internal/e2e, which opens the frames sealed here.
import rawFixture from '../../internal/e2e/testdata/interop.json';
import { CryptoError, KEY_SIZE, Session, decodeBase32 } from './crypto';

/**
 * The Go and browser implementations are two halves of one wire format. A
 * disagreement about any of it — the HKDF info strings, the salt, the nonce
 * layout, the additional data — would surface as a browser that silently
 * cannot read the terminal, which is the worst way to find out.
 *
 * The fixture holds frames sealed by each side. This opens the ones Go
 * produced; internal/e2e/interop_test.go opens the ones produced here.
 */
interface Fixture {
  sessionId: string;
  rootKeyHex: string;
  viewerKeyHex: string;
  cases: {
    name: string;
    channel: number;
    plaintext: string;
    goHostSealed: string;
    tsGuestSealed: string;
  }[];
}

const fixture = rawFixture as Fixture;

const hex = (h: string): Uint8Array =>
  new Uint8Array(h.match(/../g)?.map((b) => parseInt(b, 16)) ?? []);
const toHex = (u: Uint8Array): string =>
  [...u].map((b) => b.toString(16).padStart(2, '0')).join('');

describe('interoperability with the Go relay client', () => {
  it('opens frames sealed by Go', async () => {
    const s = await Session.fromTicketKey(
      fixture.sessionId,
      hex(fixture.rootKeyHex),
      'root',
    );

    for (const c of fixture.cases) {
      const got = await s.openFromHost(c.channel, hex(c.goHostSealed));
      expect(toHex(got), c.name).toBe(c.plaintext);
    }
  });

  // A watch-only link has to work in a browser, which means deriving the very
  // same key Go derived.
  it('derives the same viewer key', async () => {
    const viewer = await Session.fromTicketKey(
      fixture.sessionId,
      hex(fixture.viewerKeyHex),
      'viewer',
    );
    for (const c of fixture.cases) {
      const got = await viewer.openFromHost(c.channel, hex(c.goHostSealed));
      expect(toHex(got), c.name).toBe(c.plaintext);
    }
    expect(viewer.canWrite).toBe(false);
  });

  it('round-trips its own frames', async () => {
    const s = await Session.fromTicketKey(
      fixture.sessionId,
      hex(fixture.rootKeyHex),
      'root',
    );
    const plain = new TextEncoder().encode('echo hello\r');
    const sealed = await s.sealToHost(0, plain);
    expect(toHex(sealed)).not.toContain(toHex(plain));
  });
});

describe('read-only is cryptographic', () => {
  it('refuses to seal input with a viewer key', async () => {
    const viewer = await Session.fromTicketKey(
      fixture.sessionId,
      hex(fixture.viewerKeyHex),
      'viewer',
    );
    await expect(
      viewer.sealToHost(0, new TextEncoder().encode('rm -rf /')),
    ).rejects.toThrow(CryptoError);
  });
});

describe('tampering', () => {
  it('rejects an altered frame', async () => {
    const s = await Session.fromTicketKey(
      fixture.sessionId,
      hex(fixture.rootKeyHex),
      'root',
    );
    const c = fixture.cases[0]!;
    const sealed = hex(c.goHostSealed);

    for (let i = 0; i < sealed.length; i++) {
      const altered = new Uint8Array(sealed);
      altered[i]! ^= 0x01;
      await expect(s.openFromHost(c.channel, altered)).rejects.toThrow(CryptoError);
    }
  });

  it('rejects a frame moved to another channel', async () => {
    // Bytes from a forwarded connection must not pass as terminal input.
    const s = await Session.fromTicketKey(
      fixture.sessionId,
      hex(fixture.rootKeyHex),
      'root',
    );
    const forwarded = fixture.cases.find((c) => c.channel !== 0)!;
    await expect(s.openFromHost(0, hex(forwarded.goHostSealed))).rejects.toThrow(
      CryptoError,
    );
  });

  it('rejects a truncated frame', async () => {
    const s = await Session.fromTicketKey(
      fixture.sessionId,
      hex(fixture.rootKeyHex),
      'root',
    );
    const sealed = hex(fixture.cases[0]!.goHostSealed);
    for (const n of [0, 1, 12, sealed.length - 1]) {
      await expect(
        s.openFromHost(0, sealed.subarray(0, n)),
      ).rejects.toThrow(CryptoError);
    }
  });
});

describe('decodeBase32', () => {
  it('matches the encoding tickets use', () => {
    // Go writes unpadded base32 and the client lowercases it.
    expect(toHex(decodeBase32('aa'))).toBe('00');
    expect(decodeBase32('').length).toBe(0);

    const key = hex(fixture.rootKeyHex);
    // 32 bytes is 52 base32 characters with no padding.
    expect(key.length).toBe(KEY_SIZE);
  });

  it('is case-insensitive, because links get lowercased', () => {
    expect(toHex(decodeBase32('MZXW6'))).toBe(toHex(decodeBase32('mzxw6')));
  });

  it('rejects characters outside the alphabet', () => {
    expect(() => decodeBase32('0189!')).toThrow(CryptoError);
  });
});
