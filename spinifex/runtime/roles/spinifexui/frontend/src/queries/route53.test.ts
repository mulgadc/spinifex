import { beforeEach, describe, expect, it, vi } from "vitest"

const sdk = await vi.hoisted(async () => {
  const { createFakeSdk } = await import("@/test/fake-sdk")

  return createFakeSdk()
})

const activation = vi.hoisted(() => ({ get: vi.fn() }))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@/lib/route53-activation", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/route53-activation")>()),
  getHostedZoneActivation: activation.get,
}))

import { callQueryFn, callRefetchInterval } from "@/test/query"

import {
  ACTIVATION_POLL_MS,
  route53ActivationPollingQueryOptions,
  route53ActivationQueryOptions,
  route53HostedZoneQueryOptions,
  route53HostedZonesQueryOptions,
  route53RecordSetsQueryOptions,
} from "./route53"

describe("route53 queries", () => {
  beforeEach(() => {
    sdk.reset()
  })

  it("follows ListHostedZones markers to the last page", async () => {
    const markers: (string | undefined)[] = []

    sdk.setHandler("ListHostedZonesCommand", (input: { Marker?: string }) => {
      markers.push(input.Marker)

      return input.Marker
        ? {
            HostedZones: [{ Id: "/hostedzone/Z2", Name: "b.com." }],
            IsTruncated: false,
          }
        : {
            HostedZones: [{ Id: "/hostedzone/Z1", Name: "a.com." }],
            IsTruncated: true,
            NextMarker: "Z2",
          }
    })

    const zones = await callQueryFn(route53HostedZonesQueryOptions)

    expect(markers).toStrictEqual([undefined, "Z2"])
    expect(zones.map((z) => z.Name)).toStrictEqual(["a.com.", "b.com."])
  })

  it("gets one zone by its bare ID", async () => {
    sdk.setHandler("GetHostedZoneCommand", (input: { Id: string }) => ({
      HostedZone: { Id: `/hostedzone/${input.Id}` },
    }))

    const options = route53HostedZoneQueryOptions("/hostedzone/Z1")
    await callQueryFn(options)

    expect(options.queryKey).toStrictEqual(["route53", "hostedZones", "Z1"])
    expect(sdk.send.mock.calls[0]?.[0].input).toStrictEqual({ Id: "Z1" })
  })

  it("follows ListResourceRecordSets' next record to the last page", async () => {
    const inputs: unknown[] = []

    sdk.setHandler(
      "ListResourceRecordSetsCommand",
      (input: { StartRecordName?: string }) => {
        inputs.push(input)

        return input.StartRecordName
          ? {
              ResourceRecordSets: [{ Name: "www.a.com.", Type: "A" }],
              IsTruncated: false,
            }
          : {
              ResourceRecordSets: [{ Name: "a.com.", Type: "NS" }],
              IsTruncated: true,
              NextRecordName: "www.a.com.",
              NextRecordType: "A",
            }
      },
    )

    const recordSets = await callQueryFn(route53RecordSetsQueryOptions("Z1"))

    expect(inputs).toStrictEqual([
      { HostedZoneId: "Z1" },
      {
        HostedZoneId: "Z1",
        StartRecordName: "www.a.com.",
        StartRecordType: "A",
      },
    ])
    expect(recordSets).toHaveLength(2)
  })

  it("reads activation through the extension call", async () => {
    activation.get.mockResolvedValueOnce({ state: "ACTIVE" })

    await expect(
      callQueryFn(route53ActivationQueryOptions("/hostedzone/Z1")),
    ).resolves.toStrictEqual({ state: "ACTIVE" })
    expect(activation.get).toHaveBeenCalledWith("/hostedzone/Z1")
  })

  it("polls every 5 s until the zone is active", () => {
    const options = route53ActivationPollingQueryOptions("Z1")

    expect(callRefetchInterval(options, { state: "PENDING_NAMESERVERS" })).toBe(
      ACTIVATION_POLL_MS,
    )
    expect(callRefetchInterval(options, { state: "NAMESERVERS_LOST" })).toBe(
      ACTIVATION_POLL_MS,
    )
    expect(callRefetchInterval(options, { state: "ACTIVE" })).toBeFalsy()
  })
})
