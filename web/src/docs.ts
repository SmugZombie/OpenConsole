/**
 * The explainer page at /docs.
 *
 * Static prose with two live details: the relay names itself in the install
 * commands, so they can be copied without editing, and the footer reports what
 * this relay is running.
 */

import './style.css';
import './docs.css';
import { mountFooter } from './footer';

for (const node of document.querySelectorAll('[data-relay-origin]')) {
  node.textContent = window.location.origin;
}

mountFooter();
