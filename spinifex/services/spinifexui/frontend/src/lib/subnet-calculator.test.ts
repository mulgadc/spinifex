import { describe, expect, it } from "vitest"

import {
  calculateSubnetCidrs,
  cidrContains,
  cidrsOverlap,
  isValidCidr,
  numberToIp,
  parseCidr,
  subnetPrefix,
} from "./subnet-calculator"

describe("parseCidr", () => {
  it("parses a /16 CIDR", () => {
    const result = parseCidr("10.0.0.0/16")
    expect(result.network).toBe(0x0a_00_00_00)
    expect(result.prefix).toBe(16)
  })

  it("normalises non-aligned addresses", () => {
    const result = parseCidr("10.0.1.5/16")
    expect(result.network).toBe(0x0a_00_00_00)
    expect(result.prefix).toBe(16)
  })

  it("parses a /24 CIDR", () => {
    const result = parseCidr("192.168.1.0/24")
    expect(result.network).toBe(0xc0_a8_01_00)
    expect(result.prefix).toBe(24)
  })
})

describe("numberToIp", () => {
  it("converts 0x0A000000 to 10.0.0.0", () => {
    expect(numberToIp(0x0a_00_00_00)).toBe("10.0.0.0")
  })

  it("converts 0xC0A80100 to 192.168.1.0", () => {
    expect(numberToIp(0xc0_a8_01_00)).toBe("192.168.1.0")
  })
})

describe("subnetPrefix", () => {
  it("returns /20 for a /16 VPC", () => {
    expect(subnetPrefix(16)).toBe(20)
  })

  it("returns /28 (max) for a /24 VPC", () => {
    expect(subnetPrefix(24)).toBe(28)
  })
})

describe("calculateSubnetCidrs", () => {
  it("calculates correct CIDRs for 1 public + 1 private in a /16", () => {
    const result = calculateSubnetCidrs("10.0.0.0/16", 1, 1)
    expect(result.publicSubnets).toHaveLength(1)
    expect(result.privateSubnets).toHaveLength(1)
    expect(result.publicSubnets[0]?.cidr).toBe("10.0.0.0/20")
    expect(result.privateSubnets[0]?.cidr).toBe("10.0.128.0/20")
  })

  it("calculates correct CIDRs for 2 public + 2 private", () => {
    const result = calculateSubnetCidrs("10.0.0.0/16", 2, 2)
    expect(result.publicSubnets[0]?.cidr).toBe("10.0.0.0/20")
    expect(result.publicSubnets[1]?.cidr).toBe("10.0.16.0/20")
    expect(result.privateSubnets[0]?.cidr).toBe("10.0.128.0/20")
    expect(result.privateSubnets[1]?.cidr).toBe("10.0.144.0/20")
  })

  it("returns empty arrays for 0 counts", () => {
    const result = calculateSubnetCidrs("10.0.0.0/16", 0, 0)
    expect(result.publicSubnets).toHaveLength(0)
    expect(result.privateSubnets).toHaveLength(0)
  })

  it("handles 4 public + 4 private", () => {
    const result = calculateSubnetCidrs("10.0.0.0/16", 4, 4)
    expect(result.publicSubnets).toHaveLength(4)
    expect(result.privateSubnets).toHaveLength(4)
  })

  it("sets correct labels", () => {
    const result = calculateSubnetCidrs("10.0.0.0/16", 1, 1)
    expect(result.publicSubnets[0]?.label).toBe("Public Subnet 1")
    expect(result.privateSubnets[0]?.label).toBe("Private Subnet 1")
  })
})

describe("cidrsOverlap", () => {
  it("detects overlapping CIDRs", () => {
    expect(cidrsOverlap("10.0.0.0/20", "10.0.8.0/24")).toBeTruthy()
  })

  it("returns false for non-overlapping CIDRs", () => {
    expect(cidrsOverlap("10.0.0.0/20", "10.0.128.0/20")).toBeFalsy()
  })

  it("detects identical CIDRs as overlapping", () => {
    expect(cidrsOverlap("10.0.0.0/20", "10.0.0.0/20")).toBeTruthy()
  })
})

describe("cidrContains", () => {
  it("returns true when subnet fits inside VPC", () => {
    expect(cidrContains("10.0.0.0/16", "10.0.0.0/20")).toBeTruthy()
  })

  it("returns false when subnet exceeds VPC range", () => {
    expect(cidrContains("10.0.0.0/16", "10.1.0.0/20")).toBeFalsy()
  })

  it("returns true for VPC containing itself", () => {
    expect(cidrContains("10.0.0.0/16", "10.0.0.0/16")).toBeTruthy()
  })
})

describe("isValidCidr", () => {
  it("accepts valid /16 CIDR", () => {
    expect(isValidCidr("10.0.0.0/16")).toBeTruthy()
  })

  it("accepts valid /28 CIDR", () => {
    expect(isValidCidr("10.0.0.0/28")).toBeTruthy()
  })

  it("rejects /15 (too large)", () => {
    expect(isValidCidr("10.0.0.0/15")).toBeFalsy()
  })

  it("rejects /29 (too small)", () => {
    expect(isValidCidr("10.0.0.0/29")).toBeFalsy()
  })

  it("rejects invalid format", () => {
    expect(isValidCidr("not-a-cidr")).toBeFalsy()
  })

  it("rejects octets > 255", () => {
    expect(isValidCidr("256.0.0.0/16")).toBeFalsy()
  })
})
