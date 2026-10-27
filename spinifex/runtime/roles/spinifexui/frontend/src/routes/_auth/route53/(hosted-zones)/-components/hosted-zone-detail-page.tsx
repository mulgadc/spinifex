import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { ActivationBadge } from "@/components/route53/activation-badge"
import { ActivationPanel } from "@/components/route53/activation-panel"
import { RecordsTab } from "@/components/route53/records-tab"
import { Button } from "@/components/ui/button"
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
import { bareZoneId } from "@/lib/route53-activation"
import { useDeleteHostedZone } from "@/mutations/route53"
import {
  route53ActivationPollingQueryOptions,
  route53HostedZoneQueryOptions,
  route53RecordSetsQueryOptions,
} from "@/queries/route53"
import { isZoneOwnedRecord } from "@/types/route53"

interface Props {
  zoneId: string
}

function deleteErrorMessage(error: Error): string {
  return error.name === "HostedZoneNotEmpty"
    ? "The zone still holds record sets besides its NS and SOA. Delete them, then delete the zone."
    : "Failed to delete hosted zone"
}

function deleteDescription(zoneName: string, recordCount: number): string {
  const records =
    recordCount === 0
      ? ""
      : ` Its ${recordCount} record set${recordCount === 1 ? "" : "s"} besides NS and SOA will be deleted first.`

  return `Delete the hosted zone ${zoneName}?${records} This cannot be undone.`
}

export function HostedZoneDetailPage({ zoneId }: Props) {
  const id = bareZoneId(zoneId)
  const navigate = useNavigate()
  const { data: zoneData } = useSuspenseQuery(route53HostedZoneQueryOptions(id))

  const { data: recordSets } = useSuspenseQuery(
    route53RecordSetsQueryOptions(id),
  )

  const { data: activation } = useQuery(
    route53ActivationPollingQueryOptions(id),
  )

  const deleteMutation = useDeleteHostedZone()
  const [showDeleteDialog, setShowDeleteDialog] = useState(false)

  const zone = zoneData.HostedZone
  const zoneName = zone?.Name ?? id

  const customerRecords = recordSets.filter(
    (recordSet) => !isZoneOwnedRecord(recordSet, zoneName),
  )

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync({
        zoneId: id,
        recordSets: customerRecords,
      })
      await navigate({ to: "/route53/list-hosted-zones" })
    } catch {
      // surfaced via mutation.error
    } finally {
      setShowDeleteDialog(false)
    }
  }

  return (
    <>
      <BackLink to="/route53/list-hosted-zones">Back to hosted zones</BackLink>

      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg={deleteErrorMessage(deleteMutation.error)}
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <Button
                onClick={() => {
                  setShowDeleteDialog(true)
                }}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete zone
              </Button>
              <ActivationBadge state={activation?.state} />
            </div>
          }
          subtitle="Hosted zone"
          title={zoneName}
        />

        <DetailCard>
          <DetailCard.Header>Hosted zone</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Domain name" value={zoneName} />
            <DetailRow label="Hosted zone ID" value={id} />
            <DetailRow
              label="Record sets"
              value={String(zone?.ResourceRecordSetCount ?? recordSets.length)}
            />
            <DetailRow label="Comment" value={zone?.Config?.Comment} />
          </DetailCard.Content>
        </DetailCard>

        <Tabs defaultValue="records">
          <TabsList>
            <TabsTab value="records">Records</TabsTab>
            <TabsTab value="activation">Activation</TabsTab>
          </TabsList>

          <TabsPanel value="records">
            <RecordsTab
              activationState={activation?.state}
              zoneId={id}
              zoneName={zoneName}
            />
          </TabsPanel>

          <TabsPanel value="activation">
            <ActivationPanel showZoneId zoneId={id} />
          </TabsPanel>
        </Tabs>
      </div>

      <DeleteConfirmationDialog
        description={deleteDescription(zoneName, customerRecords.length)}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete hosted zone"
      />
    </>
  )
}
