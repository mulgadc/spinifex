import type { Attachment, Container, Task } from "@aws-sdk/client-ecs"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Square } from "lucide-react"
import { useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { arnResourceId, formatDateTime } from "@/lib/utils"
import { useStopTask } from "@/mutations/ecs"

import { StopTaskDialog } from "./stop-task-dialog"

function attachmentDetail(
  attachment: Attachment | undefined,
  name: string,
): string | undefined {
  return attachment?.details?.find((d) => d.name === name)?.value
}

function formatTime(value: Date | undefined): string | undefined {
  return value ? formatDateTime(value) : undefined
}

function ContainerFields({
  container,
  showGpus,
}: {
  container: Container
  showGpus: boolean
}) {
  return (
    <div>
      <p className="pt-1.5 text-sm font-medium">{container.name}</p>
      <PanelFields>
        <PanelField label="Image" mono>
          {container.image}
        </PanelField>
        <PanelField label="Status">
          <StateBadge state={container.lastStatus} />
        </PanelField>
        <PanelField label="Health">{container.healthStatus}</PanelField>
        <PanelField label="Exit code / reason">
          {container.exitCode === undefined
            ? container.reason
            : String(container.exitCode)}
        </PanelField>
        {showGpus && (
          <PanelField label="GPUs" mono>
            {container.gpuIds?.length ? container.gpuIds.join(", ") : undefined}
          </PanelField>
        )}
      </PanelFields>
    </div>
  )
}

function Overview({ clusterName, task }: { clusterName: string; task: Task }) {
  const eni = (task.attachments ?? []).find(
    (a) => a.type === "ElasticNetworkInterface",
  )

  const containers = task.containers ?? []
  const hasGpuIds = containers.some((c) => (c.gpuIds?.length ?? 0) > 0)
  const id = arnResourceId(task.taskArn)

  return (
    <>
      <PanelSection title="Configuration">
        <PanelFields>
          <PanelField copy={id} label="Task ID" mono>
            {id}
          </PanelField>
          <PanelField label="Cluster">
            <Link
              className="text-primary hover:underline"
              params={{ clusterName }}
              to="/ecs/list-clusters/$clusterName"
            >
              {clusterName}
            </Link>
          </PanelField>
          <PanelField label="Desired status">{task.desiredStatus}</PanelField>
          <PanelField label="Task definition" mono>
            {arnResourceId(task.taskDefinitionArn)}
          </PanelField>
          <PanelField label="Launch type">{task.launchType}</PanelField>
          <PanelField label="Container instance" mono>
            {task.containerInstanceArn
              ? arnResourceId(task.containerInstanceArn)
              : undefined}
          </PanelField>
          <PanelField label="Created at">
            {formatTime(task.createdAt)}
          </PanelField>
          <PanelField label="Started at">
            {formatTime(task.startedAt)}
          </PanelField>
          <PanelField label="Stopped at">
            {formatTime(task.stoppedAt)}
          </PanelField>
          {task.stoppedReason && (
            <PanelField label="Stopped reason">{task.stoppedReason}</PanelField>
          )}
        </PanelFields>
      </PanelSection>

      <PanelSection title="Networking">
        {eni ? (
          <PanelFields>
            <PanelField
              copy={attachmentDetail(eni, "networkInterfaceId")}
              label="ENI ID"
              mono
            >
              {attachmentDetail(eni, "networkInterfaceId")}
            </PanelField>
            <PanelField
              copy={attachmentDetail(eni, "privateIPv4Address")}
              label="Private IP"
              mono
            >
              {attachmentDetail(eni, "privateIPv4Address")}
            </PanelField>
            <PanelField
              copy={attachmentDetail(eni, "publicIPv4Address")}
              label="Public IP"
              mono
            >
              {attachmentDetail(eni, "publicIPv4Address")}
            </PanelField>
            <PanelField label="Subnet" mono>
              {attachmentDetail(eni, "subnetId")}
            </PanelField>
            <PanelField label="MAC" mono>
              {attachmentDetail(eni, "macAddress")}
            </PanelField>
          </PanelFields>
        ) : (
          <p className="text-sm text-muted-foreground">
            Network mode does not assign an ENI.
          </p>
        )}
      </PanelSection>

      <PanelSection title={`Containers (${containers.length})`}>
        {containers.length === 0 ? (
          <p className="text-sm text-muted-foreground">No containers.</p>
        ) : (
          <div className="flex flex-col gap-3 divide-y">
            {containers.map((container) => (
              <ContainerFields
                container={container}
                key={container.name ?? container.containerArn}
                showGpus={hasGpuIds}
              />
            ))}
          </div>
        )}
      </PanelSection>
    </>
  )
}

function Settings({ clusterName, task }: { clusterName: string; task: Task }) {
  const stopTask = useStopTask()
  const [target, setTarget] = useState<string | null>(null)

  return (
    <>
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {stopTask.isError && (
            <ErrorBanner error={stopTask.error} msg="Failed to stop task." />
          )}
          <Button
            disabled={task.lastStatus === "STOPPED" || stopTask.isPending}
            onClick={() => {
              setTarget(task.taskArn ?? null)
            }}
            size="sm"
            variant="destructive"
          >
            <Square data-icon="inline-start" />
            Stop task
          </Button>
        </div>
      </PanelSection>
      <StopTaskDialog
        clusterName={clusterName}
        mutation={stopTask}
        onOpenChange={(open) => {
          if (!open) {
            setTarget(null)
          }
        }}
        task={target}
      />
    </>
  )
}

interface TaskDetailsProps {
  clusterName: string
  task: Task
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  onClose: () => void
  layout?: "auto" | "page"
}

// One ECS task's details, as a side panel or as its own page.
export function TaskDetails({
  clusterName,
  task,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: TaskDetailsProps) {
  const id = arnResourceId(task.taskArn)

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? `Back to ${clusterName}` : "Close task details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open task page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ clusterName, taskId: id }}
            search={{ tab }}
            to="/ecs/list-clusters/$clusterName/tasks/$taskId"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Task details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={clusterName}
      summary={<StateBadge state={task.lastStatus} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview clusterName={clusterName} task={task} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings clusterName={clusterName} task={task} />,
        },
      ]}
      title={id}
    />
  )
}
