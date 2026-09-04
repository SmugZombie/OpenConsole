/**
 * The footer both pages share.
 *
 * The version is asked of the relay rather than baked into the bundle. A
 * self-hosted relay may be running a build older than whatever this page came
 * from, and the useful answer is what is actually running — that is the number
 * someone quotes in a bug report.
 */

/** Fills in the version and the copyright year, wherever they appear. */
export function mountFooter(): void {
  // The year is set here rather than written into the HTML so it cannot go
  // stale in a bundle nobody has rebuilt since December.
  for (const node of document.querySelectorAll('[data-year]')) {
    node.textContent = String(new Date().getFullYear());
  }
  void showVersion();
}

async function showVersion(): Promise<void> {
  const node = document.getElementById('relay-version');
  if (!node) return;
  try {
    const res = await fetch('/health', { headers: { Accept: 'application/json' } });
    if (!res.ok) return;
    const health = (await res.json()) as { version?: string };
    if (!health.version) return;
    node.textContent = health.version;
    node.hidden = false;
  } catch {
    // A relay that will not say leaves the slot empty. A version is a
    // courtesy, and guessing at one would be worse than omitting it.
  }
}
