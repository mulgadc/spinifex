import { describe, expect, it } from "vitest"

import { activeNavItem, breadcrumbTrail, NAV, visibleNav } from "./nav"

const [routeTreeSource = ""] = Object.values(
  import.meta.glob<string>("/src/routeTree.gen.ts", {
    eager: true,
    import: "default",
    query: "?raw",
  }),
)

// The generated tree lists every full path as a quoted literal.
const fullPaths = new Set(
  [...routeTreeSource.matchAll(/'(?<path>\/[^']*)'/gu)].map(
    (match) => match.groups?.path ?? "",
  ),
)

const items = NAV.flatMap((group) => group.items)

describe("NAV", () => {
  it("links only to routes that exist", () => {
    const missing = items
      .flatMap((item): string[] =>
        item.create ? [item.to, item.create.to] : [item.to],
      )
      .filter((to) => !fullPaths.has(to))

    expect(missing).toStrictEqual([])
  })

  it("gives every item a unique destination", () => {
    const destinations = items.map((item) => item.to)

    expect(new Set(destinations).size).toBe(destinations.length)
  })
})

describe("activeNavItem", () => {
  it("resolves each item's own page to that item", () => {
    const wrong = items
      .filter((item) => activeNavItem(NAV, item.to)?.item !== item)
      .map((item) => item.to)

    expect(wrong).toStrictEqual([])
  })

  it("keeps a detail page under its list item", () => {
    expect(
      activeNavItem(NAV, "/ec2/describe-instances/i-0a1b2c3d")?.item.label,
    ).toBe("Instances")
  })

  it("maps a create flow named in match to its list item", () => {
    expect(activeNavItem(NAV, "/ec2/run-instances")?.item.label).toBe(
      "Instances",
    )
  })

  it("matches whole path segments only", () => {
    expect(activeNavItem(NAV, "/ec2/describe-instances-extra")).toBeUndefined()
  })

  it("highlights the dashboard only on the root path", () => {
    expect(activeNavItem(NAV, "/")?.item.label).toBe("Dashboard")
    expect(activeNavItem(NAV, "/unknown")).toBeUndefined()
  })
})

describe("visibleNav", () => {
  it("hides admin items from a tenant", () => {
    const labels = visibleNav({ isAdmin: false, ochreEnabled: true }).flatMap(
      (group) => group.items.map((item) => item.label),
    )

    expect(labels).not.toContain("Nodes")
    expect(labels).not.toContain("Model Catalog")
    expect(labels).toContain("Instances")
  })

  it("hides the Ochre group while the flag is off", () => {
    const groups = visibleNav({ isAdmin: true, ochreEnabled: false }).map(
      (group) => group.label,
    )

    expect(groups).not.toContain("Ochre")
    expect(groups).toContain("EC2")
  })
})

describe("breadcrumbTrail", () => {
  it("ends at the list page on the list itself", () => {
    expect(breadcrumbTrail(NAV, "/ec2/describe-instances")).toStrictEqual([
      { label: "EC2" },
      { label: "Instances" },
    ])
  })

  it("links back to the list from a detail page and names the resource", () => {
    expect(
      breadcrumbTrail(NAV, "/ec2/describe-instances/i-0a1b2c3d"),
    ).toStrictEqual([
      { label: "EC2" },
      { label: "Instances", to: "/ec2/describe-instances" },
      { label: "i-0a1b2c3d" },
    ])
  })

  it("names a create flow by its action", () => {
    expect(breadcrumbTrail(NAV, "/ec2/run-instances").at(-1)).toStrictEqual({
      label: "Launch instance",
    })
  })

  it("decodes an encoded segment", () => {
    expect(breadcrumbTrail(NAV, "/s3/ls/my%20bucket").at(-1)).toStrictEqual({
      label: "my bucket",
    })
  })

  it("is empty off the nav", () => {
    expect(breadcrumbTrail(NAV, "/login")).toStrictEqual([])
  })
})
