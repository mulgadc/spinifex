import type { Vpc } from "@aws-sdk/client-ec2"
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
import { useDeleteVpc } from "@/mutations/ec2"
import { ec2SubnetsQueryOptions } from "@/queries/ec2"

function Overview({ vpc }: { vpc: Vpc }) {
  const { data } = useQuery(ec2SubnetsQueryOptions)
  const subnets = data?.Subnets?.filter((s) => s.VpcId === vpc.VpcId) ?? []

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={vpc.VpcId} label="VPC ID" mono>
            {vpc.VpcId}
          </PanelField>
          <PanelField copy={vpc.CidrBlock} label="CIDR" mono>
            {vpc.CidrBlock}
          </PanelField>
          <PanelField label="Default">
            {vpc.IsDefault ? "Yes" : "No"}
          </PanelField>
          <PanelField label="Tenancy">{vpc.InstanceTenancy}</PanelField>
          <PanelField label="DHCP options" mono>
            {vpc.DhcpOptionsId}
          </PanelField>
          <PanelField label="Owner" mono>
            {vpc.OwnerId}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <PanelSection title={`Subnets (${subnets.length})`}>
        {subnets.length === 0 ? (
          <p className="text-sm text-muted-foreground">No subnets.</p>
        ) : (
          <PanelFields>
            {subnets.map((subnet) => (
              <PanelField
                key={subnet.SubnetId}
                label={subnet.AvailabilityZone ?? "—"}
                mono
              >
                <Link
                  className="text-primary hover:underline"
                  params={{ id: subnet.SubnetId ?? "" }}
                  to="/ec2/describe-subnets/$id"
                >
                  {subnet.CidrBlock}
                </Link>
              </PanelField>
            ))}
          </PanelFields>
        )}
      </PanelSection>
    </>
  )
}

function Settings({ vpc, onDeleted }: { vpc: Vpc; onDeleted: () => void }) {
  const id = vpc.VpcId ?? ""
  const remove = useDeleteVpc()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Ec2Tags id={vpc.VpcId} invalidate={["ec2", "vpcs"]} tags={vpc.Tags} />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          <Button
            disabled={remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete VPC
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${id}? It must have no subnets or internet gateways left. This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete VPC"
      />
    </>
  )
}

interface VpcDetailsProps {
  vpc: Vpc
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One VPC's details, as the list's side panel or as its own page.
export function VpcDetails({
  vpc,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: VpcDetailsProps) {
  const id = vpc.VpcId ?? ""
  const name = getNameTag(vpc.Tags)

  return (
    <DetailPanel
      closeLabel={layout === "page" ? "Back to VPCs" : "Close VPC details"}
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open VPC page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-vpcs/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="VPC details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={<StateBadge state={vpc.State} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview vpc={vpc} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} vpc={vpc} />,
        },
      ]}
      title={name ?? id}
    />
  )
}
