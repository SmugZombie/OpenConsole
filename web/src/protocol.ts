/**
 * The OpenConsole wire protocol, browser side.
 *
 * This mirrors Go's `internal/protocol` exactly, and the two must be changed
 * together. See docs/protocol.md for the framing rules and why they are what
 * they are.
 *
 * Two encodings, selected by frame type:
 *   DATA      -> binary: [type:uint8][channel:uint32 BE][payload...]
 *   otherwise -> JSON envelope: {"type":"RESIZE","payload":{...}}
 *
 * Over a WebSocket that maps cleanly onto binary and text messages, so the
 * message boundary is the frame boundary and no length prefix is needed.
 */

export const PROTOCOL_VERSION = 1;

/** Size of the fixed header on a binary frame. */
export const BINARY_HEADER_LEN = 5;

/** Frame types, named as they appear in the JSON envelope. */
export type FrameType =
  | 'OPEN'
  | 'DATA'
  | 'RESIZE'
  | 'PING'
  | 'PONG'
  | 'CLOSE'
  | 'ERROR';

/**
 * Numeric type values, used only by the binary encoding. They are stable
 * across versions; new types are appended and existing values never reused.
 */
const TYPE_VALUES: Record<FrameType, number> = {
  OPEN: 1,
  DATA: 2,
  RESIZE: 3,
  PING: 4,
  PONG: 5,
  CLOSE: 6,
  ERROR: 7,
};

/** The implicit channel used until multiplexing exists. */
export const CHANNEL_CONTROL = 0;

export interface Frame {
  type: FrameType;
  channel: number;
  /** Raw bytes for DATA; the decoded JSON payload for everything else. */
  payload: Uint8Array | unknown;
}

/** Payload of an OPEN frame. */
export interface OpenPayload {
  version: number;
  session_id: string;
  role: 'host' | 'guest' | 'viewer';
  token?: string;
  cols?: number;
  rows?: number;
}

/** Payload of a RESIZE frame. */
export interface ResizePayload {
  cols: number;
  rows: number;
}

/** Payload of a CLOSE frame. */
export interface ClosePayload {
  reason?: string;
  exit_code?: number;
}

/** Payload of an ERROR frame. */
export interface ErrorPayload {
  code: string;
  message?: string;
}

export class ProtocolError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'ProtocolError';
  }
}

/**
 * Encodes terminal bytes as a binary DATA frame.
 *
 * Terminal traffic is arbitrary binary and sits on the per-keystroke latency
 * path, which is why it never goes through JSON.
 */
export function encodeData(
  payload: Uint8Array,
  channel = CHANNEL_CONTROL,
): Uint8Array {
  const out = new Uint8Array(BINARY_HEADER_LEN + payload.length);
  out[0] = TYPE_VALUES.DATA;
  new DataView(out.buffer).setUint32(1, channel, false); // big-endian
  out.set(payload, BINARY_HEADER_LEN);
  return out;
}

/** Decodes a binary frame. Only DATA uses this encoding. */
export function decodeBinary(buf: ArrayBuffer): Frame {
  if (buf.byteLength < BINARY_HEADER_LEN) {
    throw new ProtocolError(
      `binary frame is ${buf.byteLength} bytes, need at least ${BINARY_HEADER_LEN}`,
    );
  }
  const view = new DataView(buf);
  const type = view.getUint8(0);
  if (type !== TYPE_VALUES.DATA) {
    // Control frames must arrive as JSON; accepting them here would give a
    // peer two ways to say the same thing.
    throw new ProtocolError(`binary frame type ${type}, want DATA`);
  }
  return {
    type: 'DATA',
    channel: view.getUint32(1, false),
    payload: new Uint8Array(buf, BINARY_HEADER_LEN),
  };
}

/** Encodes a control frame as its JSON envelope. */
export function encodeControl(
  type: Exclude<FrameType, 'DATA'>,
  payload?: unknown,
  channel = CHANNEL_CONTROL,
): string {
  const envelope: Record<string, unknown> = { type };
  // `omitempty` on the Go side drops zero values, so match it: an envelope
  // that round-trips byte for byte is far easier to diff in a capture.
  if (channel !== CHANNEL_CONTROL) envelope.channel = channel;
  if (payload !== undefined) envelope.payload = payload;
  return JSON.stringify(envelope);
}

/** Decodes a control frame from its JSON envelope. */
export function decodeControl(text: string): Frame {
  let envelope: unknown;
  try {
    envelope = JSON.parse(text);
  } catch (err) {
    throw new ProtocolError(`malformed control message: ${String(err)}`);
  }
  if (typeof envelope !== 'object' || envelope === null) {
    throw new ProtocolError('control message must be an object');
  }

  const { type, channel, payload } = envelope as {
    type?: unknown;
    channel?: unknown;
    payload?: unknown;
  };
  if (typeof type !== 'string' || !(type in TYPE_VALUES)) {
    throw new ProtocolError(`unknown control type ${JSON.stringify(type)}`);
  }
  if (type === 'DATA') {
    // A peer must not smuggle terminal bytes through the JSON path.
    throw new ProtocolError('DATA is not valid in a control message');
  }
  return {
    type: type as FrameType,
    channel: typeof channel === 'number' ? channel : CHANNEL_CONTROL,
    payload,
  };
}
