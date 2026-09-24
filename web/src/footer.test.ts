import { describe, expect, it } from 'vitest';

import { footerVersion } from './footer';

describe('footerVersion', () => {
  it('shows the client release the relay advertises', () => {
    expect(footerVersion({ version: 'dev', client_version: 'v0.3.3' })).toBe('v0.3.3');
  });

  it("falls back to the relay's own build when it does not advertise one", () => {
    expect(footerVersion({ version: 'v0.3.1' })).toBe('v0.3.1');
  });

  it('shows nothing when the relay says nothing', () => {
    expect(footerVersion({})).toBe('');
  });
});
