import type { PlacementGroup } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2 } from "lucide-react"
import { useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Ec2Tags } from "@/components/resource-tags"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { getNameTag } from "@/lib/utils"
import { useDeletePlacementGroup } from "@/mutations/ec2"
import { ec2InstancesQueryOptions } from "@/queries/ec2"

// Instances name their group by name, not ID, in Placement.GroupName.
function useGroupInstances(groupName: string | undefined) {
  const { data } = useQuery(ec2InstancesQueryOptions)

  return (data?.Reservations ?? [])
    .flatMap((reservation) => reservation.Instances ?? [])
    .filter(
      (instance) => !!groupName && instance.Placement?.GroupName === groupName,
    )
}

function Instances({ pg }: { pg: PlacementGroup }) {
  const instances = useGroupInstances(pg.GroupName)

  return (
    <PanelSection title="Instances">
      {instances.length ? (
        <ul className="divide-y text-sm">
          {instances.map((instance) => {
            const id = instance.InstanceId ?? ""
            const name = getNameTag(instance.Tags)

            return (
              <li
                className="flex items-center justify-between gap-3 py-1.5"
                key={id}
              >
                <div className="min-w-0">
                  <Link
                    className="block truncate font-mono text-xs text-primary hover:underline"
                    params={{ id }}
                    to="/ec2/describe-instances/$id"
                  >
                    {name ?? id}
                  </Link>
                  <span className="block truncate text-xs text-muted-foreground">
                    {[
                      instance.InstanceType,
                      instance.Placement?.AvailabilityZone,
                    ]
                      .filter(Boolean)
                      .join(" · ")}
                  </span>
                </div>
                <StateBadge state={instance.State?.Name} />
              </li>
            )
          })}
        </ul>
      ) : (
        <p className="text-sm text-muted-foreground">
          No instances in this placement group.
        </p>
      )}
    </PanelSection>
  )
}

function Overview({ pg }: { pg: PlacementGroup }) {
  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={pg.GroupId} label="Group ID" mono>
            {pg.GroupId}
          </PanelField>
          <PanelField label="Group name">{pg.GroupName}</PanelField>
          <PanelField label="State">{pg.State}</PanelField>
          <PanelField label="Strategy">{pg.Strategy}</PanelField>
          {pg.Strategy === "spread" && (
            <PanelField label="Spread level">{pg.SpreadLevel}</PanelField>
          )}
          {pg.Strategy === "partition" && (
            <PanelField label="Partitions">{pg.PartitionCount}</PanelField>
          )}
          <PanelField copy={pg.GroupArn} label="ARN" mono>
            {pg.GroupArn}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <Instances pg={pg} />
    </>
  )
}

function Settings({
  pg,
  onDeleted,
}: {
  pg: PlacementGroup
  onDeleted: () => void
}) {
  const groupName = pg.GroupName ?? ""

  const inUse = useGroupInstances(pg.GroupName).some(
    (instance) => instance.State?.Name !== "terminated",
  )

  const remove = useDeletePlacementGroup()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Ec2Tags
        id={pg.GroupId}
        invalidate={["ec2", "placementGroups"]}
        tags={pg.Tags}
      />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {inUse && (
            <p className="text-sm text-muted-foreground">
              Terminate or move its instances before deleting it.
            </p>
          )}
          <Button
            disabled={!groupName || remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete placement group
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete the placement group "${groupName}"? It must have no instances in it. This cannot be undone.`}
        identifier={groupName}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete placement group"
      />
    </>
  )
}

interface PlacementGroupDetailsProps {
  pg: PlacementGroup
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One placement group's details, as the list's side panel or as its own page.
export function PlacementGroupDetails({
  pg,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: PlacementGroupDetailsProps) {
  const id = pg.GroupId ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to placement groups"
          : "Close placement group details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open placement group page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-placement-groups/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Placement group details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={pg.GroupName ? id : undefined}
      summary={<StateBadge state={pg.State} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview pg={pg} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} pg={pg} />,
        },
      ]}
      title={pg.GroupName ?? id}
    />
  )
}
