import { describe, expect, it, vi } from "vitest"

const mockSend = vi.hoisted(() => vi.fn())

vi.mock("@/lib/aws-client", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { callQueryFn } from "@/test/query"

import {
  MAP_PAGE_LIMIT,
  MAP_PAGE_SIZE,
  networkMapQueryOptions,
  readPages,
} from "./topology"

describe("readPages", () => {
  it("follows markers until a page has none", async () => {
    const markers: (string | undefined)[] = []

    const result = await readPages(async (marker) => {
      markers.push(marker)

      return await Promise.resolve(
        marker === "2" ? { items: [3] } : { items: [1], next: "2" },
      )
    })

    expect(markers).toStrictEqual([undefined, "2"])
    expect(result).toStrictEqual({ items: [1, 3], incomplete: false })
  })

  it("stops at the page limit and says a marker remains", async () => {
    let reads = 0

    const result = await readPages(async () => {
      reads += 1

      return await Promise.resolve({ items: [reads], next: "more" })
    })

    expect(reads).toBe(MAP_PAGE_LIMIT)
    expect(result.incomplete).toBeTruthy()
    expect(result.items).toHaveLength(MAP_PAGE_LIMIT)
  })
})

describe("networkMapQueryOptions", () => {
  it("filters by VPC where the API can, and names what it could not read", async () => {
    mockSend.mockImplementation(
      async (command: { constructor: { name: string } }) => {
        const { name } = command.constructor

        if (name === "DescribeDBInstancesCommand") {
          throw new Error("rds unavailable")
        }

        return await Promise.resolve(
          name === "DescribeInstancesCommand"
            ? { Reservations: [], NextToken: "next" }
            : {},
        )
      },
    )

    const result = await callQueryFn(networkMapQueryOptions("vpc-1"))

    const subnets = mockSend.mock.calls.find(
      ([c]) => c.constructor.name === "DescribeSubnetsCommand",
    )?.[0]

    const igws = mockSend.mock.calls.find(
      ([c]) => c.constructor.name === "DescribeInternetGatewaysCommand",
    )?.[0]

    expect(subnets.input.Filters).toStrictEqual([
      { Name: "vpc-id", Values: ["vpc-1"] },
    ])
    expect(igws.input.Filters).toStrictEqual([
      { Name: "attachment.vpc-id", Values: ["vpc-1"] },
    ])
    expect(result.incomplete).toStrictEqual(["instances"])
    expect(result.failed).toStrictEqual(["dbInstances"])
  })

  it("asks for a fixed page size wherever the API takes one", async () => {
    mockSend.mockResolvedValue({})
    await callQueryFn(networkMapQueryOptions("vpc-1"))

    const sizes = Object.fromEntries(
      mockSend.mock.calls.map(([c]) => [
        c.constructor.name,
        c.input.MaxResults ?? c.input.PageSize ?? c.input.MaxRecords,
      ]),
    )

    expect(sizes).toStrictEqual({
      DescribeAddressesCommand: undefined,
      DescribeDBInstancesCommand: MAP_PAGE_SIZE,
      DescribeInstancesCommand: MAP_PAGE_SIZE,
      DescribeInternetGatewaysCommand: MAP_PAGE_SIZE,
      DescribeLoadBalancersCommand: MAP_PAGE_SIZE,
      DescribeNatGatewaysCommand: MAP_PAGE_SIZE,
      DescribeRouteTablesCommand: MAP_PAGE_SIZE,
      DescribeSubnetsCommand: MAP_PAGE_SIZE,
    })
  })
})
