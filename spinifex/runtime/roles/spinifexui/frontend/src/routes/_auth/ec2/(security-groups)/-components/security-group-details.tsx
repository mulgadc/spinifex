import type { SecurityGroup } from "@aws-sdk/client-ec2"
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
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { useDeleteSecurityGroup } from "@/mutations/ec2"

import { SecurityGroupRules } from "./security-group-rules"

function Overview({ sg }: { sg: SecurityGroup }) {
  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={sg.GroupId} label="Group ID" mono>
            {sg.GroupId}
          </PanelField>
          <PanelField label="Group name">{sg.GroupName}</PanelField>
          <PanelField label="Description">
            {sg.Description && (
              <span title={sg.Description}>{sg.Description}</span>
            )}
          </PanelField>
          <PanelField label="VPC" mono>
            {sg.VpcId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: sg.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {sg.VpcId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Owner" mono>
            {sg.OwnerId}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <SecurityGroupRules direction="ingress" sg={sg} />
      <SecurityGroupRules direction="egress" sg={sg} />
    </>
  )
}

function Settings({
  sg,
  onDeleted,
}: {
  sg: SecurityGroup
  onDeleted: () => void
}) {
  const id = sg.GroupId ?? ""
  const remove = useDeleteSecurityGroup()
  const [confirm, setConfirm] = useState(false)
  // AWS refuses to delete a VPC's default group, so the action is not offered.
  const isDefault = sg.GroupName === "default"

  return (
    <>
      <PanelTags tags={sg.Tags} />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {isDefault && (
            <p className="text-sm text-muted-foreground">
              The default security group cannot be deleted.
            </p>
          )}
          <Button
            disabled={isDefault || remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete security group
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${id}? No instance or other group may still reference it. This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete security group"
      />
    </>
  )
}

interface SecurityGroupDetailsProps {
  sg: SecurityGroup
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One security group's details, as the list's side panel or as its own page.
export function SecurityGroupDetails({
  sg,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: SecurityGroupDetailsProps) {
  const id = sg.GroupId ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to security groups"
          : "Close security group details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open security group page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            to="/ec2/describe-security-groups/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Security group details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={sg.GroupName ? id : undefined}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview sg={sg} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} sg={sg} />,
        },
      ]}
      title={sg.GroupName ?? id}
    />
  )
}
