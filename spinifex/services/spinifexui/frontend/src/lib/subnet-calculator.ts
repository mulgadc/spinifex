// oxlint-disable no-bitwise
// oxlint-disable unicorn/prefer-math-trunc
// oxlint-disable no-plusplus
/**
 * Subnet CIDR auto-calculation from a VPC CIDR block.
 *
 * Strategy: public subnets start from the beginning of the VPC range,
 * private subnets start from the midpoint. Each subnet gets a /20 within
 * a /16 VPC (adjusted proportionally for other prefix lengths).
 */

/** Parse a CIDR string into its network address (as a 32-bit number) and prefix length. */
export function parseCidr(cidr: string): { network: number; prefix: number } {
  const parts = cidr.split("/")
  const ip = parts[0] ?? ""
  const prefixStr = parts[1] ?? "0"
  const prefix = Math.trunc(Number(prefixStr))
  const octets = ip.split(".").map((o) => Math.trunc(Number(o)))
  const o0 = octets[0] ?? 0
  const o1 = octets[1] ?? 0
  const o2 = octets[2] ?? 0
  const o3 = octets[3] ?? 0
  const network = ((o0 << 24) | (o1 << 16) | (o2 << 8) | o3) >>> 0
  // Mask to the prefix length to normalise
  const mask = prefix === 0 ? 0 : (0xff_ff_ff_ff << (32 - prefix)) >>> 0
  return { network: (network & mask) >>> 0, prefix }
}

/** Convert a 32-bit number back to dotted-quad notation. */
export function numberToIp(n: number): string {
  return `${(n >>> 24) & 0xff}.${(n >>> 16) & 0xff}.${(n >>> 8) & 0xff}.${n & 0xff}`
}

/** Return the number of host addresses in a CIDR block. */
export function cidrSize(prefix: number): number {
  return 2 ** (32 - prefix)
}

/**
 * Calculate subnet prefix length given the VPC prefix and total number of
 * subnets (public + private). Each half of the address space is divided among
 * its subnet type, with each subnet getting a /20 for a /16 VPC. The prefix
 * is capped so subnets never exceed /28.
 */
export function subnetPrefix(vpcPrefix: number): number {
  // Each subnet gets a /20 in a /16 VPC -> offset of 4 bits from VPC prefix
  const sub = vpcPrefix + 4
  return Math.min(sub, 28)
}

export interface SubnetCidr {
  cidr: string
  label: string
}

/**
 * Calculate default subnet CIDRs for a VPC.
 *
 * Public subnets occupy the first half of the VPC address space,
 * private subnets occupy the second half.
 */
export function calculateSubnetCidrs(
  vpcCidr: string,
  publicCount: number,
  privateCount: number,
): { publicSubnets: SubnetCidr[]; privateSubnets: SubnetCidr[] } {
  const { network, prefix: vpcPrefix } = parseCidr(vpcCidr)
  const subPrefix = subnetPrefix(vpcPrefix)
  const subSize = cidrSize(subPrefix)
  const halfOffset = cidrSize(vpcPrefix) / 2

  const publicSubnets: SubnetCidr[] = []
  for (let i = 0; i < publicCount; i++) {
    const addr = (network + i * subSize) >>> 0
    publicSubnets.push({
      cidr: `${numberToIp(addr)}/${subPrefix}`,
      label: `Public Subnet ${i + 1}`,
    })
  }

  const privateSubnets: SubnetCidr[] = []
  for (let i = 0; i < privateCount; i++) {
    const addr = (network + halfOffset + i * subSize) >>> 0
    privateSubnets.push({
      cidr: `${numberToIp(addr)}/${subPrefix}`,
      label: `Private Subnet ${i + 1}`,
    })
  }

  return { publicSubnets, privateSubnets }
}

/** Check whether two CIDR blocks overlap. */
export function cidrsOverlap(a: string, b: string): boolean {
  const cidrA = parseCidr(a)
  const cidrB = parseCidr(b)
  const endA = (cidrA.network + cidrSize(cidrA.prefix) - 1) >>> 0
  const endB = (cidrB.network + cidrSize(cidrB.prefix) - 1) >>> 0
  return cidrA.network <= endB && cidrB.network <= endA
}

/** Check that a subnet CIDR fits entirely within a VPC CIDR. */
export function cidrContains(vpcCidr: string, subnetCidr: string): boolean {
  const vpc = parseCidr(vpcCidr)
  const sub = parseCidr(subnetCidr)
  const vpcEnd = (vpc.network + cidrSize(vpc.prefix) - 1) >>> 0
  const subEnd = (sub.network + cidrSize(sub.prefix) - 1) >>> 0
  return sub.network >= vpc.network && subEnd <= vpcEnd
}

/** Validate a CIDR string for basic format and prefix range. */
export function isValidCidr(
  cidr: string,
  minPrefix = 16,
  maxPrefix = 28,
): boolean {
  const match =
    /^(?<octet1>\d{1,3})\.(?<octet2>\d{1,3})\.(?<octet3>\d{1,3})\.(?<octet4>\d{1,3})\/(?<prefix>\d{1,2})$/.exec(
      cidr,
    )
  if (!match?.groups) {
    return false
  }
  const { octet1, octet2, octet3, octet4, prefix } = match.groups
  const octets = [
    Math.trunc(Number(octet1 ?? "0")),
    Math.trunc(Number(octet2 ?? "0")),
    Math.trunc(Number(octet3 ?? "0")),
    Math.trunc(Number(octet4 ?? "0")),
  ]
  if (octets.some((o) => o > 255)) {
    return false
  }
  const prefixBits = Math.trunc(Number(prefix ?? "0"))
  return prefixBits >= minPrefix && prefixBits <= maxPrefix
}
