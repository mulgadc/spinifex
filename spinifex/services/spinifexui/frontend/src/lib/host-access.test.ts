import { describe, expect, it } from "vitest"

import { isDirectHostAccess } from "./host-access"

describe("isDirectHostAccess", () => {
  it("is true for loopback and bare addresses", () => {
    expect(isDirectHostAccess("localhost")).toBeTruthy()
    expect(isDirectHostAccess("LOCALHOST")).toBeTruthy()
    expect(isDirectHostAccess("127.0.0.1")).toBeTruthy()
    expect(isDirectHostAccess("72.52.77.230")).toBeTruthy()
    expect(isDirectHostAccess("::1")).toBeTruthy()
    expect(isDirectHostAccess("[2001:db8::1]")).toBeTruthy()
  })

  // These reach the cluster through a proxy holding a publicly trusted
  // certificate, so the cluster CA is irrelevant to them.
  it("is false for named hosts", () => {
    expect(isDirectHostAccess("console.spx3.com")).toBeFalsy()
    expect(isDirectHostAccess("spx3.com")).toBeFalsy()
  })

  // A name that merely starts with digits is still a name.
  it("does not mistake a numeric-looking name for an address", () => {
    expect(isDirectHostAccess("10.example.com")).toBeFalsy()
    expect(isDirectHostAccess("1.2.3.4.example.com")).toBeFalsy()
  })
})
