import type { DBSubnetGroup } from "@aws-sdk/client-rds"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2 } from "lucide-react"
import { useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"

import { RdsTagsTab } from "../../-components/rds-tags-tab"
import { DeleteDBSubnetGroupDialog } from "./delete-db-subnet-group-dialog"

function Overview({ group }: { group: DBSubnetGroup }) {
  const subnets = group.Subnets ?? []

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={group.DBSubnetGroupName} label="Name" mono>
            {group.DBSubnetGroupName}
          </PanelField>
          <PanelField label="Description">
            {group.DBSubnetGroupDescription}
          </PanelField>
          <PanelField label="VPC" mono>
            {group.VpcId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: group.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {group.VpcId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Status">{group.SubnetGroupStatus}</PanelField>
          <PanelField copy={group.DBSubnetGroupArn} label="ARN" mono>
            {group.DBSubnetGroupArn}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <PanelSection title={`Subnets (${subnets.length})`}>
        {subnets.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            This group holds no subnets.
          </p>
        ) : (
          <PanelFields>
            {subnets.map((subnet) => (
              <PanelField
                key={subnet.SubnetIdentifier}
                label={subnet.SubnetAvailabilityZone?.Name ?? "—"}
                mono
              >
                <Link
                  className="text-primary hover:underline"
                  params={{ id: subnet.SubnetIdentifier ?? "" }}
                  to="/ec2/describe-subnets/$id"
                >
                  {subnet.SubnetIdentifier}
                </Link>
                {subnet.SubnetStatus && (
                  <span className="text-muted-foreground">
                    {" "}
                    · {subnet.SubnetStatus}
                  </span>
                )}
              </PanelField>
            ))}
          </PanelFields>
        )}
        <p className="mt-2 text-xs text-muted-foreground">
          This group records which subnets RDS may place an endpoint in; each
          subnet&apos;s own CIDR, addresses and routing live on its EC2 page.
          This console cannot edit membership; use aws rds
          modify-db-subnet-group. A running DB instance keeps the subnet its
          endpoint was placed in.
        </p>
      </PanelSection>
    </>
  )
}

function Settings({
  group,
  onDeleted,
}: {
  group: DBSubnetGroup
  onDeleted: () => void
}) {
  const name = group.DBSubnetGroupName ?? ""
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelSection title="Tags">
        <RdsTagsTab arn={group.DBSubnetGroupArn ?? ""} />
      </PanelSection>
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          <Button
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete subnet group
          </Button>
        </div>
      </PanelSection>
      <DeleteDBSubnetGroupDialog
        dbSubnetGroupName={name}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
      />
    </>
  )
}

interface DBSubnetGroupDetailsProps {
  group: DBSubnetGroup
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One DB subnet group, as the list's side panel or as its own page.
export function DBSubnetGroupDetails({
  group,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: DBSubnetGroupDetailsProps) {
  const name = group.DBSubnetGroupName ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to subnet groups"
          : "Close subnet group details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open subnet group page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ name }}
            search={{ tab }}
            to="/rds/describe-db-subnet-groups/$name"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="DB subnet group details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      summary={<StateBadge state={group.SubnetGroupStatus} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview group={group} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings group={group} onDeleted={onClose} />,
        },
      ]}
      title={name}
    />
  )
}
