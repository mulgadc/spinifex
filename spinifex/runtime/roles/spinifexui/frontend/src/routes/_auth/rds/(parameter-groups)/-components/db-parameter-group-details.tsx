import type { DBParameterGroup, Parameter } from "@aws-sdk/client-rds"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2 } from "lucide-react"
import { useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelSpinner,
} from "@/components/detail-panel"
import { Badge } from "@/components/ui/badge"
import { Button, buttonVariants } from "@/components/ui/button"
import { useWideLayout } from "@/hooks/use-wide-layout"
import type { PanelTab } from "@/lib/panel-search"
import { rdsParametersQueryOptions } from "@/queries/rds"
import { isDefaultParameterGroupName, PARAMETER_SOURCE_USER } from "@/types/rds"

import { RdsTagsTab } from "../../-components/rds-tags-tab"
import { DeleteDBParameterGroupDialog } from "./delete-db-parameter-group-dialog"
import { ParametersEditor } from "./parameters-editor"

function DefaultGroupNote() {
  return (
    <div
      className="rounded-md border border-warning-border bg-warning-subtle p-3 text-sm"
      role="note"
    >
      <p className="font-medium">This is a default parameter group</p>
      <p className="mt-1 text-xs text-muted-foreground">
        The service owns it. It can be read and attached to an instance, but it
        cannot be modified, deleted or tagged. Create your own group of the same
        family to change a value.
      </p>
    </div>
  )
}

function Overview({
  group,
  parameters,
}: {
  group: DBParameterGroup
  parameters: Parameter[] | undefined
}) {
  const name = group.DBParameterGroupName ?? ""
  const isDefault = isDefaultParameterGroupName(name)

  const overrides =
    parameters?.filter((p) => p.Source === PARAMETER_SOURCE_USER) ?? []

  return (
    <>
      {isDefault && <DefaultGroupNote />}
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={name} label="Name" mono>
            {name}
          </PanelField>
          <PanelField label="Family" mono>
            {group.DBParameterGroupFamily}
          </PanelField>
          <PanelField label="Description">{group.Description}</PanelField>
          <PanelField label="Type">
            {isDefault ? "Default" : "Customer"}
          </PanelField>
          <PanelField copy={group.DBParameterGroupArn} label="ARN" mono>
            {group.DBParameterGroupArn}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <PanelSection title={`Overridden values (${overrides.length})`}>
        {parameters === undefined && <PanelSpinner />}
        {parameters !== undefined &&
          (overrides.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              Every parameter holds its engine default.
            </p>
          ) : (
            <PanelFields>
              {overrides.map((p) => (
                <PanelField
                  key={p.ParameterName}
                  label={p.ParameterName ?? ""}
                  mono
                >
                  {p.ParameterValue}
                </PanelField>
              ))}
            </PanelFields>
          ))}
      </PanelSection>
    </>
  )
}

function Settings({
  group,
  parameters,
  inPanel,
  onDeleted,
}: {
  group: DBParameterGroup
  parameters: Parameter[] | undefined
  // The editor is a wide table, so the side panel links to the page for it.
  inPanel: boolean
  onDeleted: () => void
}) {
  const name = group.DBParameterGroupName ?? ""
  const isDefault = isDefaultParameterGroupName(name)
  // A default group has no stored record, so ListTagsForResource has nothing
  // to answer for it and tags are not offered.
  const arn = isDefault ? "" : (group.DBParameterGroupArn ?? "")
  const [confirm, setConfirm] = useState(false)

  let editor = <PanelSpinner />

  if (inPanel) {
    editor = (
      <div className="flex flex-col items-start gap-3">
        <p className="text-sm text-muted-foreground">
          {parameters === undefined
            ? "Parameters load on the group's page."
            : `${parameters.length} parameters. The editor needs the full width, so it lives on the group's page.`}
        </p>
        <Link
          className={buttonVariants({ size: "sm", variant: "outline" })}
          params={{ name }}
          search={{ tab: "settings" }}
          to="/rds/describe-db-parameter-groups/$name"
        >
          {isDefault ? "View parameters" : "Edit parameters"}
        </Link>
      </div>
    )
  } else if (parameters !== undefined) {
    editor = (
      <ParametersEditor
        dbParameterGroupName={name}
        parameters={parameters}
        readOnly={isDefault}
      />
    )
  }

  return (
    <>
      <PanelSection title="Parameters">{editor}</PanelSection>
      <PanelSection title="Tags">
        {isDefault ? (
          <p className="text-sm text-muted-foreground">
            A default group cannot be tagged.
          </p>
        ) : (
          <RdsTagsTab arn={arn} />
        )}
      </PanelSection>
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {isDefault && (
            <p className="text-sm text-muted-foreground">
              A default group is owned by the service and cannot be deleted.
            </p>
          )}
          <Button
            disabled={isDefault}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete parameter group
          </Button>
        </div>
      </PanelSection>
      <DeleteDBParameterGroupDialog
        dbParameterGroupName={name}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
      />
    </>
  )
}

interface DBParameterGroupDetailsProps {
  group: DBParameterGroup
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One DB parameter group, as the list's side panel or as its own page.
export function DBParameterGroupDetails({
  group,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: DBParameterGroupDetailsProps) {
  const name = group.DBParameterGroupName ?? ""
  const isDefault = isDefaultParameterGroupName(name)
  const wide = useWideLayout()
  const { data } = useQuery(rdsParametersQueryOptions(name))
  const parameters = data?.Parameters

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to parameter groups"
          : "Close parameter group details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open parameter group page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ name }}
            search={{ tab }}
            to="/rds/describe-db-parameter-groups/$name"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="DB parameter group details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={group.DBParameterGroupFamily}
      summary={
        <Badge variant="secondary">{isDefault ? "Default" : "Customer"}</Badge>
      }
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview group={group} parameters={parameters} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: (
            <Settings
              group={group}
              inPanel={layout === "auto" && wide}
              onDeleted={onClose}
              parameters={parameters}
            />
          ),
        },
      ]}
      title={name}
    />
  )
}
