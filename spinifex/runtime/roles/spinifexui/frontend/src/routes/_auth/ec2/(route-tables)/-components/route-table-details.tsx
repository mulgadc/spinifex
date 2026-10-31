import type { RouteTable } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2 } from "lucide-react"
import { useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelTags,
} from "@/components/detail-panel"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Badge } from "@/components/ui/badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { getNameTag } from "@/lib/utils"
import { useDeleteRouteTable } from "@/mutations/ec2"

import { RouteTableAssociations } from "./route-table-associations"
import { RouteTableRoutes } from "./route-table-routes"

export function isMainRouteTable(rtb: RouteTable): boolean {
  return rtb.Associations?.some((a) => a.Main) ?? false
}

function Overview({ rtb }: { rtb: RouteTable }) {
  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={rtb.RouteTableId} label="Route table ID" mono>
            {rtb.RouteTableId}
          </PanelField>
          <PanelField label="Main">
            {isMainRouteTable(rtb) ? "Yes" : "No"}
          </PanelField>
          <PanelField label="VPC" mono>
            {rtb.VpcId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: rtb.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {rtb.VpcId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Owner" mono>
            {rtb.OwnerId}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <RouteTableRoutes rtb={rtb} />
      <RouteTableAssociations rtb={rtb} />
    </>
  )
}

function Settings({
  rtb,
  onDeleted,
}: {
  rtb: RouteTable
  onDeleted: () => void
}) {
  const id = rtb.RouteTableId ?? ""
  const main = isMainRouteTable(rtb)
  const remove = useDeleteRouteTable()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelTags tags={rtb.Tags} />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {main && (
            <p className="text-sm text-muted-foreground">
              A VPC&apos;s main route table is deleted with the VPC.
            </p>
          )}
          <Button
            disabled={main || remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete route table
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${id}? This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete route table"
      />
    </>
  )
}

interface RouteTableDetailsProps {
  rtb: RouteTable
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One route table's details, as the list's side panel or as its own page.
export function RouteTableDetails({
  rtb,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: RouteTableDetailsProps) {
  const id = rtb.RouteTableId ?? ""
  const name = getNameTag(rtb.Tags)

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to route tables" : "Close route table details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open route table page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-route-tables/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Route table details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={
        isMainRouteTable(rtb) ? <Badge variant="secondary">Main</Badge> : null
      }
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview rtb={rtb} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} rtb={rtb} />,
        },
      ]}
      title={name ?? id}
    />
  )
}
