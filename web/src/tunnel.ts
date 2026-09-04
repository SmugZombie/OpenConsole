/**
 * The browser end of an OpenConsole tunnel.
 *
 * Everything transport-specific lives here, the same way `internal/tunnel`
 * isolates it on the Go side: the UI in main.ts deals in terminal bytes and
 * connection states, never in WebSocket frames.
 */

import { CryptoError, type Session } from './crypto';
import {
  CHANNEL_CONTROL,
  PROTOCOL_VERSION,
  ProtocolError,
  decodeBinary,
  decodeControl,
  encodeControl,
  encodeData,
  type ClosePayload,
  type ErrorPayload,
  type Frame,
  type OpenPayload,
  type ResizePayload,
} from './protocol';

export type Status =
  | 'connecting'
  | 'connected'
  | 'closed'
  | 'error';

export interface TunnelHandlers {
  /** Terminal output arrived. */
  onData(bytes: Uint8Array): void;
  /** The host's terminal changed size. */
  onResize(size: ResizePayload): void;
  /** Connection state changed; `detail` is a human-readable reason. */
  onStatus(status: Status, detail?: string): void;
  /**
   * The relay said what this connection may do. Called once, on attach.
   *
   * The answer comes from the relay's reading of the token, not from anything
   * this page asked for, so a viewer link is read-only no matter what the
   * client does.
   */
  onAccess(readOnly: boolean): void;
}

/** How a session ended, so the UI can say something useful. */
export interface Ending {
  reason: string;
  /** True when the host ended the session normally, rather than a failure. */
  graceful: boolean;
}

export interface TunnelOptions {
  sessionId: string;
  token: string;
  cols: number;
  rows: number;
  handlers: TunnelHandlers;
  /** Encryption keys from the ticket, absent for an unencrypted session. */
  crypto?: Session | undefined;
}

/** Maps the relay's error codes onto something worth reading. */
function describeError(e: ErrorPayload): string {
  switch (e.code) {
    case 'unauthorized':
      return 'This link is not valid, or the session has expired.';
    case 'session_not_found':
      return 'No live terminal here — the host may have disconnected.';
    case 'unsupported_version':
      return 'This page speaks a different protocol version than the relay. Try a reload.';
    case 'protocol_error':
      return `The relay rejected the connection${e.message ? `: ${e.message}` : '.'}`;
    default:
      return e.message || e.code || 'The relay refused the connection.';
  }
}

/** Builds the tunnel URL from the page's own origin. */
export function tunnelURL(): string {
  const url = new URL('/api/v1/tunnel', window.location.href);
  // A page served over https must not open an insecure ws:// tunnel; browsers
  // block it as mixed content anyway, but failing loudly here is clearer.
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  return url.toString();
}

export class Tunnel {
  private ws: WebSocket | null = null;
  private readonly opts: TunnelOptions;
  private opened = false;
  private readOnly = false;
  private ending: Ending | null = null;

  /**
   * Decryption is asynchronous, and terminal output must not be reordered:
   * two frames decrypted concurrently could finish in either order and put
   * the wrong bytes on the screen. Chaining every frame onto this promise
   * keeps them in the order they arrived.
   */
  private inbound: Promise<void> = Promise.resolve();

  constructor(opts: TunnelOptions) {
    this.opts = opts;
  }

  /** How the session ended, once it has. */
  get endedWith(): Ending | null {
    return this.ending;
  }

  connect(): void {
    const { handlers } = this.opts;
    handlers.onStatus('connecting');

    let ws: WebSocket;
    try {
      ws = new WebSocket(tunnelURL());
    } catch (err) {
      this.fail(`Could not open a connection: ${String(err)}`);
      return;
    }
    ws.binaryType = 'arraybuffer';
    this.ws = ws;

    ws.onopen = () => {
      // The credential goes in the OPEN frame, never the URL, so it stays out
      // of access logs and browser history.
      this.send(
        encodeControl('OPEN', {
          version: PROTOCOL_VERSION,
          session_id: this.opts.sessionId,
          role: 'guest',
          token: this.opts.token,
          cols: this.opts.cols,
          rows: this.opts.rows,
        }),
      );
    };

    ws.onmessage = (ev: MessageEvent) => {
      let frame: Frame;
      try {
        frame =
          typeof ev.data === 'string'
            ? decodeControl(ev.data)
            : decodeBinary(ev.data as ArrayBuffer);
      } catch (err) {
        this.fail(
          err instanceof ProtocolError ? `Protocol error: ${err.message}` : String(err),
        );
        return;
      }

      // Queued rather than awaited inline, so frames reach the terminal in
      // the order they arrived even though decryption is asynchronous.
      this.inbound = this.inbound.then(async () => {
        try {
          await this.handle(frame);
        } catch (err) {
          if (err instanceof CryptoError) {
            // Failing closed: showing whatever bytes arrived would be
            // showing something the relay could have chosen.
            this.fail(
              'A frame did not decrypt. The link may be wrong, or the relay may be interfering.',
            );
            return;
          }
          this.fail(String(err));
        }
      });
    };

    ws.onerror = () => {
      // The browser deliberately withholds the reason for a WebSocket failure,
      // so there is nothing more specific to report than the fact of it.
      if (!this.ending) this.fail('Connection failed.');
    };

    ws.onclose = () => {
      if (this.ending) {
        this.opts.handlers.onStatus(
          this.ending.graceful ? 'closed' : 'error',
          this.ending.reason,
        );
        return;
      }
      if (!this.opened) {
        this.fail('The relay closed the connection before the session started.');
        return;
      }
      this.ending = { reason: 'The connection was lost.', graceful: false };
      this.opts.handlers.onStatus('error', this.ending.reason);
    };
  }

  private async handle(frame: Frame): Promise<void> {
    const { handlers } = this.opts;

    switch (frame.type) {
      case 'OPEN': {
        // The relay acknowledges with OPEN only once the connection is
        // genuinely attached to a live terminal, so this is the point at which
        // there is something to show.
        this.opened = true;
        const ack = (frame.payload ?? {}) as OpenPayload;
        // Anything other than an explicit "guest" is treated as read-only:
        // erring towards the narrower capability means a relay that says
        // something unexpected cannot accidentally hand over the keyboard.
        // A link that lost its key would otherwise fill the terminal with
        // ciphertext. Say what happened; a truncated link is the usual cause.
        if (ack.encrypted && !this.opts.crypto) {
          this.fail(
            'This session is end-to-end encrypted but this link carries no key. ' +
              'Ask for the whole link — the part after the # has two halves.',
          );
          return;
        }
        if (!ack.encrypted && this.opts.crypto) {
          this.fail(
            'This link carries a key but the relay says the session is not encrypted. ' +
              'Do not continue: someone may be trying to read this terminal.',
          );
          return;
        }

        // A ticket with no writing key is read-only whatever the relay says,
        // so the narrower of the two answers wins.
        this.readOnly = ack.role !== 'guest' || this.opts.crypto?.canWrite === false;
        handlers.onAccess(this.readOnly);
        handlers.onStatus('connected');
        break;
      }

      case 'DATA': {
        const payload = frame.payload as Uint8Array;
        const crypto = this.opts.crypto;
        handlers.onData(
          crypto ? await crypto.openFromHost(frame.channel, payload) : payload,
        );
        break;
      }

      case 'RESIZE': {
        const size = frame.payload as ResizePayload;
        if (typeof size?.cols === 'number' && typeof size?.rows === 'number') {
          handlers.onResize(size);
        }
        break;
      }

      case 'PING':
        this.send(encodeControl('PONG'));
        break;

      case 'PONG':
        break;

      case 'CLOSE': {
        const c = (frame.payload ?? {}) as ClosePayload;
        this.ending = {
          reason: c.reason ? `Session ended: ${c.reason}.` : 'The host ended the session.',
          graceful: true,
        };
        this.close();
        break;
      }

      case 'ERROR': {
        const e = (frame.payload ?? {}) as ErrorPayload;
        this.fail(describeError(e));
        break;
      }
    }
  }

  /** True when the relay granted watch-only access. */
  get isReadOnly(): boolean {
    return this.readOnly;
  }

  /** Sends a keystroke, or anything else the user typed, to the terminal. */
  write(bytes: Uint8Array): void {
    // The relay drops a viewer's input anyway; not sending it keeps the
    // intent visible here and saves the round trip.
    if (this.readOnly) return;
    if (this.ws?.readyState !== WebSocket.OPEN) return;

    const crypto = this.opts.crypto;
    if (!crypto) {
      this.send(encodeData(bytes));
      return;
    }
    // Keystrokes are sealed in the order they were typed, for the same reason
    // output is decrypted in order.
    this.outbound = this.outbound
      .then(async () => {
        const sealed = await crypto.sealToHost(CHANNEL_CONTROL, bytes);
        this.send(encodeData(sealed));
      })
      .catch(() => {
        /* a closed session; the read side reports it */
      });
  }

  private outbound: Promise<void> = Promise.resolve();

  private send(payload: string | Uint8Array): void {
    this.ws?.send(payload);
  }

  private fail(reason: string): void {
    if (!this.ending) this.ending = { reason, graceful: false };
    this.opts.handlers.onStatus('error', reason);
    this.close();
  }

  close(): void {
    const ws = this.ws;
    this.ws = null;
    if (ws && ws.readyState <= WebSocket.OPEN) {
      ws.close(1000, 'closed by client');
    }
  }
}
