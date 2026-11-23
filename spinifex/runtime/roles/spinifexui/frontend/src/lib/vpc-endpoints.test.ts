import { describe, expect, it } from "vitest"

import {
  compactPolicy,
  FULL_ACCESS_POLICY,
  isFullAccessPolicy,
  nameTagSpecificationArg,
  routeTableChanges,
  routeTableDetail,
} from "./vpc-endpoints"

describe("isFullAccessPolicy", () => {
  it.each([
    ["no document", undefined, true],
    ["the default document", FULL_ACCESS_POLICY, true],
    [
      "the default as one statement object, in another key order",
      '{"Statement":{"Resource":"*","Action":"*","Principal":"*","Effect":"Allow"}}',
      true,
    ],
    [
      "a narrower resource",
      '{"Statement":[{"Effect":"Allow","Principal":"*","Action":"*","Resource":"arn:aws:s3:::logs/*"}]}',
      false,
    ],
    [
      "the default with a condition",
      '{"Statement":[{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*","Condition":{}}]}',
      false,
    ],
    [
      "two statements",
      '{"Statement":[{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*"},{"Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"*"}]}',
      false,
    ],
    ["malformed JSON", "{", false],
  ])("is %s: %s", (_, document, full) => {
    expect(isFullAccessPolicy(document)).toBe(full)
  })
})

describe("routeTableChanges", () => {
  it("adds and removes only the difference", () => {
    expect(
      routeTableChanges(["rtb-a", "rtb-b"], ["rtb-b", "rtb-c"]),
    ).toStrictEqual({ add: ["rtb-c"], remove: ["rtb-a"] })
  })

  it("sends nothing when the set is unchanged in another order", () => {
    expect(
      routeTableChanges(["rtb-a", "rtb-b"], ["rtb-b", "rtb-a"]),
    ).toStrictEqual({ add: [], remove: [] })
  })
})

describe("routeTableDetail", () => {
  it("names the table, marks the main one and counts its subnets", () => {
    expect(
      routeTableDetail({
        Tags: [{ Key: "Name", Value: "private" }],
        Associations: [
          { Main: true },
          { SubnetId: "s-1" },
          { SubnetId: "s-2" },
        ],
      }),
    ).toBe("private · main · 2 subnets")
  })

  it("is empty for an unnamed table with no associations", () => {
    expect(routeTableDetail({})).toBe("")
  })
})

describe("compactPolicy", () => {
  it("drops the editor's layout, and leaves text that is not JSON as typed", () => {
    expect(compactPolicy(FULL_ACCESS_POLICY)).toBe(
      '{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*"}]}',
    )
    expect(compactPolicy("{ nope")).toBe("{ nope")
  })
})

describe("nameTagSpecificationArg", () => {
  it("keeps a quote and a comma in the name inside one shell argument", () => {
    expect(nameTagSpecificationArg("it's, mine")).toBe(
      `'[{"ResourceType":"vpc-endpoint","Tags":[{"Key":"Name","Value":"it'"'"'s, mine"}]}]'`,
    )
  })
})
