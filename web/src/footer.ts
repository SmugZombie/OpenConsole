/**
 * The footer both pages share.
 *
 * The version is asked of the relay rather than baked into the bundle, which
 * may have been built long before or after whatever the relay is running.
 *
 * What is shown is the client release the relay tells clients to run. The
 * relay keeps that current by itself, so it is a real release number even on
 * a relay deployed without one of its own, where the relay's build reads
 * "dev". A relay too old to report it falls back to its own build.
 */

/** The fields of /health the footer reads. */
export interface HealthVersions {
  version?: string;
  client_version?: string;
}

/** Picks the version to show from /health, or '' for none. */
export function footerVersion(health: HealthVersions): string {
  return health.client_version || health.version || '';
}

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
    const version = footerVersion((await res.json()) as HealthVersions);
    if (!version) return;
    node.textContent = version;
    node.hidden = false;
  } catch {
    // A relay that will not say leaves the slot empty. A version is a
    // courtesy, and guessing at one would be worse than omitting it.
  }
}
