/**
 * End-to-end encryption, browser side.
 *
 * This mirrors Go's `internal/e2e` exactly and the two must change together.
 * Every constant here — the HKDF info strings, the nonce length, the additional
 * data — is part of the wire format, not an implementation detail.
 *
 * AES-256-GCM and HKDF-SHA256 are used because WebCrypto implements both
 * natively. Shipping a JavaScript cipher to get a different primitive would
 * trade a vetted implementation for an unvetted one, in the one place where
 * that matters most.
 */

/** Length of the root key and of each derived key. */
export const KEY_SIZE = 32;

/** GCM's standard nonce, and its tag. */
const NONCE_SIZE = 12;
const TAG_SIZE = 16;

const INFO_HOST_TO_GUEST = 'openconsole/v1 host-to-guest';
const INFO_GUEST_TO_HOST = 'openconsole/v1 guest-to-host';

export class CryptoError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'CryptoError';
  }
}

/** What a ticket's key letter says the key is for. */
export type KeyKind = 'root' | 'viewer';

/**
 * Session holds the keys for one shared terminal.
 *
 * `guestToHost` is absent for a viewer, which is what makes watch-only
 * cryptographic rather than a rule the relay is trusted to apply.
 */
export class Session {
  private constructor(
    private readonly hostToGuest: CryptoKey,
    private readonly guestToHost: CryptoKey | null,
  ) {}

  /** Builds the keys a ticket allows. */
  static async fromTicketKey(
    sessionId: string,
    key: Uint8Array,
    kind: KeyKind,
  ): Promise<Session> {
    if (key.length !== KEY_SIZE) {
      throw new CryptoError(`key is ${key.length} bytes, expected ${KEY_SIZE}`);
    }
    if (kind === 'viewer') {
      // A viewer is handed the read direction's key directly; there is
      // nothing to derive, and nothing it could derive the other from.
      return new Session(await importKey(key), null);
    }
    const [out, inbound] = await Promise.all([
      deriveKey(sessionId, key, INFO_HOST_TO_GUEST),
      deriveKey(sessionId, key, INFO_GUEST_TO_HOST),
    ]);
    return new Session(out, inbound);
  }

  /** True when this participant can produce input the host will accept. */
  get canWrite(): boolean {
    return this.guestToHost !== null;
  }

  /** Decrypts terminal output. */
  async openFromHost(channel: number, sealed: Uint8Array): Promise<Uint8Array> {
    return open(this.hostToGuest, channel, sealed);
  }

  /** Encrypts input for the host. */
  async sealToHost(channel: number, plaintext: Uint8Array): Promise<Uint8Array> {
    if (!this.guestToHost) {
      throw new CryptoError('this session is read-only');
    }
    return seal(this.guestToHost, channel, plaintext);
  }
}

/** HKDF-SHA256 with the session ID as salt, matching the Go side. */
async function deriveKey(
  sessionId: string,
  root: Uint8Array,
  info: string,
): Promise<CryptoKey> {
  const material = await crypto.subtle.importKey('raw', toBuffer(root), 'HKDF', false, [
    'deriveBits',
  ]);
  const bits = await crypto.subtle.deriveBits(
    {
      name: 'HKDF',
      hash: 'SHA-256',
      salt: toBuffer(new TextEncoder().encode(sessionId)),
      info: toBuffer(new TextEncoder().encode(info)),
    },
    material,
    KEY_SIZE * 8,
  );
  return importKey(new Uint8Array(bits));
}

async function importKey(raw: Uint8Array): Promise<CryptoKey> {
  return crypto.subtle.importKey('raw', toBuffer(raw), 'AES-GCM', false, [
    'encrypt',
    'decrypt',
  ]);
}

/** Produces nonce || ciphertext || tag. */
async function seal(
  key: CryptoKey,
  channel: number,
  plaintext: Uint8Array,
): Promise<Uint8Array> {
  // Random rather than a counter, because several guests share this key and
  // would otherwise have to agree on who uses which counter.
  const nonce = crypto.getRandomValues(new Uint8Array(NONCE_SIZE));
  const ciphertext = new Uint8Array(
    await crypto.subtle.encrypt(
      { name: 'AES-GCM', iv: toBuffer(nonce), additionalData: toBuffer(channelAAD(channel)) },
      key,
      toBuffer(plaintext),
    ),
  );

  const out = new Uint8Array(nonce.length + ciphertext.length);
  out.set(nonce, 0);
  out.set(ciphertext, nonce.length);
  return out;
}

async function open(
  key: CryptoKey,
  channel: number,
  sealed: Uint8Array,
): Promise<Uint8Array> {
  if (sealed.length < NONCE_SIZE + TAG_SIZE) {
    throw new CryptoError('frame is too short to be authentic');
  }
  try {
    const plain = await crypto.subtle.decrypt(
      {
        name: 'AES-GCM',
        iv: toBuffer(sealed.subarray(0, NONCE_SIZE)),
        additionalData: toBuffer(channelAAD(channel)),
      },
      key,
      toBuffer(sealed.subarray(NONCE_SIZE)),
    );
    return new Uint8Array(plain);
  } catch {
    // Deliberately uninformative, like the Go side: a caller must not be able
    // to tell a corrupted frame from a forged one.
    throw new CryptoError('could not decrypt');
  }
}

/**
 * Binds a frame to its channel, so bytes from a forwarded connection cannot be
 * passed off as terminal input, or the reverse.
 */
function channelAAD(channel: number): Uint8Array {
  const aad = new Uint8Array(4);
  new DataView(aad.buffer).setUint32(0, channel >>> 0, false); // big-endian
  return aad;
}

/** Copies into a plain ArrayBuffer, which is what WebCrypto accepts. */
function toBuffer(a: Uint8Array): ArrayBuffer {
  return a.slice().buffer as ArrayBuffer;
}

/** Encodes bytes as the unpadded lowercase base32 the tickets use. */
export function encodeBase32(bytes: Uint8Array): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let out = '';
  let bits = 0;
  let value = 0;
  for (const b of bytes) {
    value = (value << 8) | b;
    bits += 8;
    while (bits >= 5) {
      bits -= 5;
      out += alphabet[(value >> bits) & 31];
    }
  }
  if (bits > 0) {
    out += alphabet[(value << (5 - bits)) & 31];
  }
  return out.toLowerCase();
}

/**
 * Decodes the unpadded, lowercase base32 the tickets use.
 *
 * Base32 rather than base64 so a whole ticket can be read aloud or typed; this
 * has to match Go's base32.StdEncoding with padding removed.
 */
export function decodeBase32(s: string): Uint8Array {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  const input = s.trim().toUpperCase();

  const out: number[] = [];
  let bits = 0;
  let value = 0;
  for (const ch of input) {
    const idx = alphabet.indexOf(ch);
    if (idx < 0) {
      throw new CryptoError(`"${ch}" is not valid base32`);
    }
    value = (value << 5) | idx;
    bits += 5;
    if (bits >= 8) {
      bits -= 8;
      out.push((value >> bits) & 0xff);
    }
  }
  return new Uint8Array(out);
}
