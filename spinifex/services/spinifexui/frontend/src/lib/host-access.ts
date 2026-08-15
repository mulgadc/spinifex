// A cluster reached by name sits behind a proxy holding a publicly trusted
// certificate, so its users never see a warning and have no reason to install
// the cluster CA. Only direct access — localhost or a bare address — hits the
// self-signed certificate the CA exists for.

const LOOPBACK_NAMES = new Set(["localhost", "127.0.0.1", "::1"])
const IPV4 = /^\d{1,3}(?:\.\d{1,3}){3}$/

export function isDirectHostAccess(hostname: string): boolean {
  const host = hostname.toLowerCase().replaceAll(/^\[|]$/g, "")
  if (LOOPBACK_NAMES.has(host)) {
    return true
  }
  if (IPV4.test(host)) {
    return true
  }
  // Any IPv6 literal: a hostname cannot contain a colon.
  return host.includes(":")
}
