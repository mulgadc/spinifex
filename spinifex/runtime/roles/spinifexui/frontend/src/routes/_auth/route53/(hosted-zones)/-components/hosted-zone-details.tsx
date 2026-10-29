import type { HostedZone } from "@aws-sdk/client-route-53"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2 } from "lucide-react"
import { Suspense, useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelSpinner,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { ActivationBadge } from "@/components/route53/activation-badge"
import { ActivationPanel } from "@/components/route53/activation-panel"
import { RecordsTab } from "@/components/route53/records-tab"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { bareZoneId } from "@/lib/route53-activation"
import { useDeleteHostedZone } from "@/mutations/route53"
import {
  route53ActivationPollingQueryOptions,
  route53RecordSetsQueryOptions,
} from "@/queries/route53"
import { isZoneOwnedRecord } from "@/types/route53"

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

function Overview({ zone }: { zone: HostedZone }) {
  const id = bareZoneId(zone.Id ?? "")
  const zoneName = zone.Name ?? id

  const { data: activation } = useQuery(
    route53ActivationPollingQueryOptions(id),
  )

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField label="Domain name">{zoneName}</PanelField>
          <PanelField copy={id} label="Hosted zone ID" mono>
            {id}
          </PanelField>
          <PanelField label="Record sets">
            {zone.ResourceRecordSetCount?.toString()}
          </PanelField>
          <PanelField label="Comment">{zone.Config?.Comment}</PanelField>
        </PanelFields>
      </PanelSection>
      <PanelSection title="Records">
        <Suspense fallback={<PanelSpinner />}>
          <RecordsTab
            activationState={activation?.state}
            zoneId={id}
            zoneName={zoneName}
          />
        </Suspense>
      </PanelSection>
      <ActivationPanel zoneId={id} />
    </>
  )
}

// Deleting the zone deletes the customer's records first, so it reads them.
function DeleteZone({
  zone,
  onDeleted,
}: {
  zone: HostedZone
  onDeleted: () => void
}) {
  const id = bareZoneId(zone.Id ?? "")
  const zoneName = zone.Name ?? id

  const { data: recordSets } = useSuspenseQuery(
    route53RecordSetsQueryOptions(id),
  )

  const remove = useDeleteHostedZone()
  const [confirm, setConfirm] = useState(false)

  const customerRecords = recordSets.filter(
    (recordSet) => !isZoneOwnedRecord(recordSet, zoneName),
  )

  async function onDelete() {
    try {
      await remove.mutateAsync({ zoneId: id, recordSets: customerRecords })
      onDeleted()
    } catch {
      // Shown by the banner above.
    } finally {
      setConfirm(false)
    }
  }

  return (
    <div className="flex flex-col items-start gap-3">
      {remove.error && (
        <ErrorBanner
          error={remove.error}
          msg={deleteErrorMessage(remove.error)}
        />
      )}
      <Button
        disabled={remove.isPending}
        onClick={() => {
          setConfirm(true)
        }}
        size="sm"
        variant="destructive"
      >
        <Trash2 data-icon="inline-start" />
        Delete zone
      </Button>
      <DeleteConfirmationDialog
        description={deleteDescription(zoneName, customerRecords.length)}
        isPending={remove.isPending}
        onConfirm={() => {
          void onDelete()
        }}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete hosted zone"
      />
    </div>
  )
}

function Settings({
  zone,
  onDeleted,
}: {
  zone: HostedZone
  onDeleted: () => void
}) {
  return (
    <PanelSection title="Manage">
      <Suspense fallback={<PanelSpinner />}>
        <DeleteZone onDeleted={onDeleted} zone={zone} />
      </Suspense>
    </PanelSection>
  )
}

function Summary({ zoneId }: { zoneId: string }) {
  const { data: activation, isPending } = useQuery(
    route53ActivationPollingQueryOptions(zoneId),
  )

  return (
    <ActivationBadge
      fallback={isPending ? "Loading…" : "Unknown"}
      state={activation?.state}
    />
  )
}

interface HostedZoneDetailsProps {
  zone: HostedZone
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One hosted zone's details, as the list's side panel or as its own page.
export function HostedZoneDetails({
  zone,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: HostedZoneDetailsProps) {
  const id = bareZoneId(zone.Id ?? "")

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to hosted zones" : "Close hosted zone details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open hosted zone page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            to="/route53/list-hosted-zones/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Hosted zone details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      size="wide"
      subtitle={id}
      summary={<Summary zoneId={id} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview zone={zone} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} zone={zone} />,
        },
      ]}
      title={zone.Name ?? id}
    />
  )
}
