import {
  GetHostedZoneCommand,
  type HostedZone,
  ListHostedZonesCommand,
  ListResourceRecordSetsCommand,
  type ResourceRecordSet,
  type RRType,
} from "@aws-sdk/client-route-53"
import { queryOptions } from "@tanstack/react-query"

import { getRoute53Client } from "@/lib/awsClient"
import {
  bareZoneId,
  getHostedZoneActivation,
  type HostedZoneActivation,
} from "@/lib/route53-activation"

export const ACTIVATION_POLL_MS = 5000

// listAllHostedZones follows ListHostedZones' marker to the last page; each
// page needs the previous one's marker, so the pages are fetched in turn.
async function listAllHostedZones(marker?: string): Promise<HostedZone[]> {
  const page = await getRoute53Client().send(
    new ListHostedZonesCommand({ Marker: marker }),
  )

  const zones = page.HostedZones ?? []

  if (!page.IsTruncated || !page.NextMarker) {
    return zones
  }

  return [...zones, ...(await listAllHostedZones(page.NextMarker))]
}

export const route53HostedZonesQueryOptions = queryOptions({
  queryKey: ["route53", "hostedZones"],
  queryFn: async () => await listAllHostedZones(),
})

export const route53HostedZoneQueryOptions = (zoneId: string) =>
  queryOptions({
    queryKey: ["route53", "hostedZones", bareZoneId(zoneId)],
    queryFn: async () =>
      await getRoute53Client().send(
        new GetHostedZoneCommand({ Id: bareZoneId(zoneId) }),
      ),
  })

interface RecordSetCursor {
  StartRecordName?: string
  StartRecordType?: RRType
}

// listAllRecordSets follows ListResourceRecordSets' next-record cursor.
async function listAllRecordSets(
  zoneId: string,
  cursor: RecordSetCursor = {},
): Promise<ResourceRecordSet[]> {
  const page = await getRoute53Client().send(
    new ListResourceRecordSetsCommand({
      HostedZoneId: bareZoneId(zoneId),
      ...cursor,
    }),
  )

  const recordSets = page.ResourceRecordSets ?? []

  if (!page.IsTruncated || !page.NextRecordName) {
    return recordSets
  }

  const rest = await listAllRecordSets(zoneId, {
    StartRecordName: page.NextRecordName,
    StartRecordType: page.NextRecordType,
  })

  return [...recordSets, ...rest]
}

export const route53RecordSetsQueryOptions = (zoneId: string) =>
  queryOptions({
    queryKey: ["route53", "hostedZones", bareZoneId(zoneId), "recordSets"],
    queryFn: async () => await listAllRecordSets(zoneId),
  })

export const route53ActivationQueryOptions = (zoneId: string) =>
  queryOptions({
    queryKey: ["route53", "hostedZones", bareZoneId(zoneId), "activation"],
    queryFn: async () => await getHostedZoneActivation(zoneId),
  })

export function activationRefetchInterval(
  activation: HostedZoneActivation | undefined,
): number | false {
  return activation?.state === "ACTIVE" ? false : ACTIVATION_POLL_MS
}

// Polls every 5 s until the zone is active, which is how the walkthrough and
// the zone page notice the parent's delegation landing.
export const route53ActivationPollingQueryOptions = (zoneId: string) =>
  queryOptions({
    ...route53ActivationQueryOptions(zoneId),
    refetchInterval: (query) => activationRefetchInterval(query.state.data),
  })
