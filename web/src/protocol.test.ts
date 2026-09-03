import { describe, expect, it } from 'vitest';

import {
  BINARY_HEADER_LEN,
  ProtocolError,
  decodeBinary,
  decodeControl,
  encodeControl,
  encodeData,
} from './protocol';

/** Helper: encode then decode, the way a round trip over the wire would. */
function roundTripBinary(payload: Uint8Array) {
  const encoded = encodeData(payload);
  return decodeBinary(
    encoded.buffer.slice(
      encoded.byteOffset,
      encoded.byteOffset + encoded.byteLength,
    ) as ArrayBuffer,
  );
}

describe('binary frames', () => {
  it('round-trips arbitrary bytes', () => {
    // NUL, an escape sequence and invalid UTF-8: precisely what JSON could not
    // carry without base64, which is why DATA is binary.
    const payload = new Uint8Array([0x00, 0x1b, 0x5b, 0x41, 0xff, 0xfe, 0x0a]);
    const frame = roundTripBinary(payload);

    expect(frame.type).toBe('DATA');
    expect(frame.channel).toBe(0);
    expect(Array.from(frame.payload as Uint8Array)).toEqual(Array.from(payload));
  });

  it('round-trips an empty payload', () => {
    const frame = roundTripBinary(new Uint8Array(0));
    expect((frame.payload as Uint8Array).length).toBe(0);
  });

  it('writes the 5-byte header Go expects', () => {
    const encoded = encodeData(new Uint8Array([0x41]));
    expect(encoded.length).toBe(BINARY_HEADER_LEN + 1);
    expect(encoded[0]).toBe(2); // DATA
    expect(Array.from(encoded.slice(1, 5))).toEqual([0, 0, 0, 0]); // channel 0
    expect(encoded[5]).toBe(0x41);
  });

  it('encodes the channel big-endian, matching the Go header', () => {
    // Multiplexing is not implemented, but the field is on the wire from v1 so
    // adding it later is not a format break.
    const encoded = encodeData(new Uint8Array(0), 0xdeadbeef);
    expect(Array.from(encoded.slice(1, 5))).toEqual([0xde, 0xad, 0xbe, 0xef]);
    const frame = decodeBinary(encoded.buffer as ArrayBuffer);
    expect(frame.channel).toBe(0xdeadbeef);
  });

  it('rejects a truncated frame', () => {
    expect(() => decodeBinary(new ArrayBuffer(4))).toThrow(ProtocolError);
  });

  it('rejects a control type sent as binary', () => {
    // Otherwise a peer would have two ways to say the same thing.
    const buf = new Uint8Array([4, 0, 0, 0, 0]); // PING
    expect(() => decodeBinary(buf.buffer as ArrayBuffer)).toThrow(ProtocolError);
  });
});

describe('control frames', () => {
  it('produces the self-describing envelope Go parses', () => {
    const text = encodeControl('RESIZE', { cols: 120, rows: 40 });
    expect(JSON.parse(text)).toEqual({
      type: 'RESIZE',
      payload: { cols: 120, rows: 40 },
    });
  });

  it('omits an absent payload and the default channel', () => {
    // Go's omitempty drops these, and a byte-identical envelope is far easier
    // to diff in a packet capture.
    expect(JSON.parse(encodeControl('PONG'))).toEqual({ type: 'PONG' });
  });

  it('round-trips an OPEN frame', () => {
    const open = {
      version: 1,
      session_id: 'x5s5gzxptgfksy3hu75jmcoltm',
      role: 'guest' as const,
      token: 'secret',
      cols: 100,
      rows: 30,
    };
    const frame = decodeControl(encodeControl('OPEN', open));
    expect(frame.type).toBe('OPEN');
    expect(frame.payload).toEqual(open);
  });

  it('decodes an ERROR frame', () => {
    const frame = decodeControl(
      '{"type":"ERROR","payload":{"code":"unauthorized","message":"nope"}}',
    );
    expect(frame.type).toBe('ERROR');
    expect(frame.payload).toEqual({ code: 'unauthorized', message: 'nope' });
  });

  it('defaults a missing channel to the control channel', () => {
    expect(decodeControl('{"type":"PING"}').channel).toBe(0);
  });

  it('rejects DATA smuggled through the JSON path', () => {
    // This would bypass the binary size accounting.
    expect(() => decodeControl('{"type":"DATA","payload":"aGk="}')).toThrow(
      ProtocolError,
    );
  });

  it('rejects malformed messages', () => {
    for (const bad of ['', '{', '[]', 'null', '{"type":123}', '{"type":"NOPE"}']) {
      expect(() => decodeControl(bad), bad).toThrow(ProtocolError);
    }
  });
});
