import type { CreateHostedZoneCommandInput } from "@aws-sdk/client-route-53"
import { waitFor } from "@testing-library/react"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderMutationHook } from "@/test/render"

const sdk = await vi.hoisted(async () => {
  const { createFakeSdk } = await import("@/test/fake-sdk")

  return createFakeSdk()
})

const activation = vi.hoisted(() => ({ verify: vi.fn() }))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@/lib/route53-activation", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/route53-activation")>()),
  verifyHostedZoneDelegation: activation.verify,
}))

import {
  useChangeResourceRecordSets,
  useCreateHostedZone,
  useDeleteHostedZone,
  useVerifyHostedZoneDelegation,
} from "./route53"

function commandNames(): string[] {
  return sdk.send.mock.calls.map(([command]) => command.constructor.name)
}

describe("route53 mutations", () => {
  beforeEach(() => {
    sdk.reset()
  })

  it("creates a zone with a fresh caller reference and the comment", async () => {
    sdk.setHandler("CreateHostedZoneCommand", () => ({}))
    const { result } = renderMutationHook(() => useCreateHostedZone())

    result.current.mutate({ name: " example.com ", comment: "prod" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })

    const input = sdk.send.mock.calls[0]?.[0].input as
      | CreateHostedZoneCommandInput
      | undefined

    expect(input).toMatchObject({
      Name: "example.com",
      HostedZoneConfig: { Comment: "prod" },
    })
    expect(input?.CallerReference).toMatch(/^[0-9a-f-]{36}$/)
  })

  it("omits the zone config when there is no comment", async () => {
    sdk.setHandler("CreateHostedZoneCommand", () => ({}))
    const { result } = renderMutationHook(() => useCreateHostedZone())

    result.current.mutate({ name: "example.com", comment: "  " })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(sdk.send.mock.calls[0]?.[0].input).not.toHaveProperty(
      "HostedZoneConfig",
    )
  })

  it("deletes the given record sets in one batch before the zone", async () => {
    sdk.setHandler("ChangeResourceRecordSetsCommand", () => ({}))
    sdk.setHandler("DeleteHostedZoneCommand", () => ({}))
    const www = { Name: "www.example.com.", Type: "A" as const, TTL: 60 }
    const { result } = renderMutationHook(() => useDeleteHostedZone())

    result.current.mutate({ zoneId: "/hostedzone/Z1", recordSets: [www] })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(commandNames()).toStrictEqual([
      "ChangeResourceRecordSetsCommand",
      "DeleteHostedZoneCommand",
    ])
    expect(sdk.send.mock.calls[0]?.[0].input).toStrictEqual({
      HostedZoneId: "Z1",
      ChangeBatch: { Changes: [{ Action: "DELETE", ResourceRecordSet: www }] },
    })
    expect(sdk.send.mock.calls[1]?.[0].input).toStrictEqual({ Id: "Z1" })
  })

  it("deletes an empty zone with no change batch", async () => {
    sdk.setHandler("DeleteHostedZoneCommand", () => ({}))
    const { result } = renderMutationHook(() => useDeleteHostedZone())

    result.current.mutate({ zoneId: "Z1" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(commandNames()).toStrictEqual(["DeleteHostedZoneCommand"])
  })

  it("sends a change batch and refreshes the zone's records", async () => {
    sdk.setHandler("ChangeResourceRecordSetsCommand", () => ({}))
    const queryClient = createTestQueryClient()
    const invalidate = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useChangeResourceRecordSets(),
      queryClient,
    )

    const changes = [
      {
        Action: "UPSERT" as const,
        ResourceRecordSet: { Name: "a.example.com.", Type: "A" as const },
      },
    ]

    result.current.mutate({ zoneId: "Z1", changes })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(sdk.send.mock.calls[0]?.[0].input).toStrictEqual({
      HostedZoneId: "Z1",
      ChangeBatch: { Changes: changes },
    })
    expect(invalidate).toHaveBeenCalledWith({
      queryKey: ["route53", "hostedZones", "Z1", "recordSets"],
    })
  })

  it("stores the activation a check now returns", async () => {
    activation.verify.mockResolvedValueOnce({ state: "ACTIVE" })

    const { result, queryClient } = renderMutationHook(() =>
      useVerifyHostedZoneDelegation(),
    )

    result.current.mutate("/hostedzone/Z1")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(
      queryClient.getQueryData(["route53", "hostedZones", "Z1", "activation"]),
    ).toStrictEqual({ state: "ACTIVE" })
  })
})
