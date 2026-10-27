import {
  type Change,
  ChangeResourceRecordSetsCommand,
  CreateHostedZoneCommand,
  DeleteHostedZoneCommand,
  type ResourceRecordSet,
} from "@aws-sdk/client-route-53"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getRoute53Client } from "@/lib/awsClient"
import {
  bareZoneId,
  verifyHostedZoneDelegation,
} from "@/lib/route53-activation"
import type { CreateHostedZoneFormData } from "@/types/route53"

const HOSTED_ZONES_KEY = ["route53", "hostedZones"] as const

export function useCreateHostedZone() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: async (data: CreateHostedZoneFormData) => {
      const comment = data.comment?.trim()

      const command = new CreateHostedZoneCommand({
        Name: data.name.trim(),
        CallerReference: crypto.randomUUID(),
        ...(comment && { HostedZoneConfig: { Comment: comment } }),
      })

      return await getRoute53Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: HOSTED_ZONES_KEY })
    },
  })
}

export interface DeleteHostedZoneParams {
  zoneId: string
  // Record sets to remove first: the API refuses to delete a zone holding any
  // record set other than its apex NS and SOA.
  recordSets?: ResourceRecordSet[]
}

export function useDeleteHostedZone() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: async ({ zoneId, recordSets = [] }: DeleteHostedZoneParams) => {
      const client = getRoute53Client()
      const id = bareZoneId(zoneId)

      if (recordSets.length > 0) {
        await client.send(
          new ChangeResourceRecordSetsCommand({
            HostedZoneId: id,
            ChangeBatch: {
              Changes: recordSets.map((recordSet) => ({
                Action: "DELETE",
                ResourceRecordSet: recordSet,
              })),
            },
          }),
        )
      }

      return await client.send(new DeleteHostedZoneCommand({ Id: id }))
    },
    // The zone's own queries are left alone: the detail page is still mounted
    // here, and refetching them would only answer NoSuchHostedZone.
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: HOSTED_ZONES_KEY,
        exact: true,
      })
    },
  })
}

export interface ChangeRecordSetsParams {
  zoneId: string
  changes: Change[]
  comment?: string
}

export function useChangeResourceRecordSets() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: async ({
      zoneId,
      changes,
      comment,
    }: ChangeRecordSetsParams) => {
      const command = new ChangeResourceRecordSetsCommand({
        HostedZoneId: bareZoneId(zoneId),
        ChangeBatch: { Changes: changes, ...(comment && { Comment: comment }) },
      })

      return await getRoute53Client().send(command)
    },
    onSuccess: (_data, { zoneId }) => {
      void queryClient.invalidateQueries({
        queryKey: [...HOSTED_ZONES_KEY, bareZoneId(zoneId), "recordSets"],
      })
      // The list's record count moves with every change.
      void queryClient.invalidateQueries({
        queryKey: HOSTED_ZONES_KEY,
        exact: true,
      })
    },
  })
}

export function useVerifyHostedZoneDelegation() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: async (zoneId: string) =>
      await verifyHostedZoneDelegation(zoneId),
    onSuccess: (activation, zoneId) => {
      queryClient.setQueryData(
        [...HOSTED_ZONES_KEY, bareZoneId(zoneId), "activation"],
        activation,
      )
    },
  })
}
