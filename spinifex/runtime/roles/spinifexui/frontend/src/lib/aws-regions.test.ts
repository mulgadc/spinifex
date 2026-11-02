import { describe, expect, it } from "vitest"

import { regionLocation } from "./aws-regions"

describe("regionLocation", () => {
  it.each([
    ["ap-southeast-2", "AU", "Sydney"],
    ["us-west-1", "US", "N. California"],
    ["eu-west-2", "GB", "London"],
    ["EU-CENTRAL-1", "DE", "Frankfurt"],
  ])("places %s", (region, country, city) => {
    expect(regionLocation(region)).toStrictEqual({ country, city })
  })

  it("has no place for a custom region name", () => {
    expect(regionLocation("home-lab")).toBeUndefined()
    expect(regionLocation(undefined)).toBeUndefined()
  })
})
