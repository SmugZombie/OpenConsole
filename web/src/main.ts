/**
 * OpenConsole browser client.
 *
 * Two views live in one page: a landing form at `/`, and a terminal at
 * `/s/<session>`. The guest token arrives in the URL *fragment*, which browsers
 * never send to a server — see parseSessionURL below.
 */

import { FitAddon } from '@xterm/addon-fit';
import { WebLinksAddon } from '@xterm/addon-web-links';
import { Terminal } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';

import './style.css';
import {
  parseSessionURL,
  parseTicket,
  sessionURL,
  type SessionLocator,
} from './locator';
import { Tunnel, type Status } from './tunnel';

/** Font size bounds when scaling the terminal to the window. */
const MIN_FONT = 6;
const MAX_FONT = 22;
const BASE_FONT = 14;

function el<T extends HTMLElement>(id: string): T {
  const found = document.getElementById(id);
  if (!found) throw new Error(`missing element #${id}`);
  return found as T;
}

/* --- landing -------------------------------------------------------------- */

function showLanding(): void {
  el('landing').hidden = false;

  const form = el<HTMLFormElement>('join-form');
  const input = el<HTMLInputElement>('ticket');
  const error = el('ticket-error');

  form.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const parsed = parseTicket(input.value);
    if (!parsed) {
      error.textContent = 'That does not look like a ticket. Expected session.token';
      error.hidden = false;
      return;
    }
    error.hidden = true;
    // Assembling the URL here — rather than posting the ticket anywhere — is
    // what keeps the token client-side.
    window.location.href = sessionURL(parsed);
  });

  input.focus();
}

/* --- session -------------------------------------------------------------- */

function showSession(locator: SessionLocator): void {
  el('session').hidden = false;
  el('session-label').textContent = locator.sessionId;

  const term = new Terminal({
    fontFamily:
      'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
    fontSize: BASE_FONT,
    cursorBlink: true,
    // The host's shell owns the scrollback; a guest only ever sees what the
    // relay replays, so a large local buffer would be misleading.
    scrollback: 5000,
    // Brand palette: near-black tile colour, amber cursor.
    theme: {
      background: '#12141a',
      foreground: '#f3efe6',
      cursor: '#e8a54b',
      cursorAccent: '#12141a',
      selectionBackground: '#e8a54b40',
    },
  });
  const fit = new FitAddon();
  term.loadAddon(fit);
  term.loadAddon(new WebLinksAddon());
  term.open(el('terminal'));

  const decoder = new TextDecoder();
  const encoder = new TextEncoder();

  // The host owns the terminal's dimensions: guests are told the size and
  // render to match, rather than resizing someone else's real window. So
  // "fitting" means picking a font size that makes the host's grid fit here,
  // not changing the number of columns.
  let hostCols = 80;
  let hostRows = 24;

  function refit(): void {
    // Two passes: changing the font size changes the cell size, so the first
    // estimate is approximate and the second lands on it. More passes buy
    // nothing measurable.
    for (let pass = 0; pass < 2; pass++) {
      const proposed = fit.proposeDimensions();
      if (!proposed || !proposed.cols || !proposed.rows) return;

      // proposeDimensions reports the grid that fits at the current font size,
      // which gives the ratio to scale the font by instead of the grid.
      const scale = Math.min(proposed.cols / hostCols, proposed.rows / hostRows);
      const next = Math.max(
        MIN_FONT,
        Math.min(MAX_FONT, Math.floor(term.options.fontSize! * scale)),
      );
      if (next === term.options.fontSize) break;
      term.options.fontSize = next;
    }
    term.resize(hostCols, hostRows);
  }

  const statusDot = el('status-dot');
  const statusText = el('status-text');
  const accessLabel = el('access-label');
  const overlay = el('overlay');
  const overlayTitle = el('overlay-title');
  const overlayBody = el('overlay-body');
  const overlayAction = el<HTMLButtonElement>('overlay-action');

  function setStatus(status: Status, detail?: string): void {
    statusDot.className = `dot ${status}`;
    switch (status) {
      case 'connecting':
        statusText.textContent = 'Connecting…';
        overlay.hidden = true;
        break;
      case 'connected':
        statusText.textContent = 'Connected';
        overlay.hidden = true;
        term.focus();
        break;
      case 'closed':
        statusText.textContent = 'Session ended';
        overlayTitle.textContent = 'Session ended';
        overlayBody.textContent = detail ?? 'The host ended the session.';
        overlayAction.hidden = true;
        overlay.hidden = false;
        break;
      case 'error':
        statusText.textContent = 'Disconnected';
        overlayTitle.textContent = 'Disconnected';
        overlayBody.textContent = detail ?? 'The connection was lost.';
        overlayAction.hidden = false;
        overlay.hidden = false;
        break;
    }
  }

  let tunnel: Tunnel;

  function connect(): void {
    tunnel = new Tunnel({
      sessionId: locator.sessionId,
      token: locator.token,
      cols: hostCols,
      rows: hostRows,
      handlers: {
        onData: (bytes) => term.write(decoder.decode(bytes, { stream: true })),
        onResize: ({ cols, rows }) => {
          hostCols = cols;
          hostRows = rows;
          refit();
        },
        onStatus: setStatus,
        onAccess: (readOnly) => {
          // Stop xterm from echoing keystrokes locally. Without this a viewer
          // would see their own typing appear and believe it had been sent.
          term.options.disableStdin = readOnly;
          term.options.cursorBlink = !readOnly;
          accessLabel.textContent = readOnly ? 'read-only' : '';
          accessLabel.hidden = !readOnly;
        },
      },
    });
    tunnel.connect();
  }

  term.onData((data) => tunnel.write(encoder.encode(data)));

  // Pasted or programmatic binary input arrives here rather than onData.
  term.onBinary((data) => {
    const bytes = new Uint8Array(data.length);
    for (let i = 0; i < data.length; i++) bytes[i] = data.charCodeAt(i) & 0xff;
    tunnel.write(bytes);
  });

  overlayAction.addEventListener('click', () => {
    term.clear();
    connect();
  });

  window.addEventListener('resize', refit);
  window.addEventListener('beforeunload', () => tunnel.close());

  refit();
  connect();
}

/* --- entry ---------------------------------------------------------------- */

// Changing only the fragment does not reload the page, so pasting a complete
// link while already on the same session path would otherwise leave whatever is
// on screen — including the "incomplete link" message — in place forever. A
// different fragment means a different credential, so start over.
window.addEventListener('hashchange', () => window.location.reload());

const locator = parseSessionURL(window.location.pathname, window.location.hash);
if (locator) {
  showSession(locator);
} else if (window.location.pathname.startsWith('/s/')) {
  // A session URL with no fragment: the link was truncated somewhere, which is
  // a common way for a chat client to break one.
  el('session').hidden = false;
  el('status-dot').className = 'dot error';
  el('status-text').textContent = 'Incomplete link';
  el('overlay').hidden = false;
  el('overlay-title').textContent = 'This link is incomplete';
  el('overlay-body').textContent =
    'The part after the # is missing, so there is no way to join. Ask for the full link.';
  el<HTMLButtonElement>('overlay-action').hidden = true;
} else {
  showLanding();
}
