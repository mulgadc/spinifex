import type { QueryClient } from "@tanstack/react-query"

import { bareZoneId } from "@/lib/route53-activation"
import type { DeleteHostedZoneParams } from "@/mutations/route53"
import { route53RecordSetsQueryOptions } from "@/queries/route53"
import { isZoneOwnedRecord } from "@/types/route53"

// Deletes a zone with the customer's record sets in it: the API refuses to
// delete a zone holding anything besides its apex NS and SOA, so those go
// first. The panel and the list's bulk delete both come through here.
export async function deleteHostedZone(
  queryClient: QueryClient,
  zoneId: string,
  zoneName: string,
  remove: (params: DeleteHostedZoneParams) => Promise<object>,
): Promise<object> {
  const id = bareZoneId(zoneId)

  const recordSets = await queryClient.query(route53RecordSetsQueryOptions(id))

  return await remove({
    zoneId: id,
    recordSets: recordSets.filter(
      (recordSet) => !isZoneOwnedRecord(recordSet, zoneName),
    ),
  })
}
