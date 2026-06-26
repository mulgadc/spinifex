import type { Attachment, Container, Task } from "@aws-sdk/client-ecs"
import { useSuspenseQuery } from "@tanstack/react-query"

import { BackLink } from "@/components/back-link"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { ecsTasksQueryOptions } from "@/queries/ecs"

// shortArn renders the trailing resource segment of an ARN, falling back to the
// whole string when there is no slash.
function shortArn(arn: string | undefined): string {
  if (!arn) {
    return ""
  }
  const idx = arn.lastIndexOf("/")
  return idx === -1 ? arn : arn.slice(idx + 1)
}

function attachmentDetail(
  attachment: Attachment | undefined,
  name: string,
): string | undefined {
  return attachment?.details?.find((d) => d.name === name)?.value
}

function formatTime(value: Date | undefined): string {
  return value ? new Date(value).toISOString() : ""
}

export function TaskDetailPage({
  clusterName,
  taskId,
}: {
  clusterName: string
  taskId: string
}) {
  const { data: tasks } = useSuspenseQuery(ecsTasksQueryOptions(clusterName))
  const task = tasks.find((t: Task) => (t.taskArn ?? "").endsWith(taskId))

  if (!task) {
    return (
      <>
        <BackLink params={{ clusterName }} to="/ecs/list-clusters/$clusterName">
          {clusterName}
        </BackLink>
        <PageHeading subtitle="Task Details" title={taskId} />
        <p className="text-muted-foreground">Task not found.</p>
      </>
    )
  }

  const eni = (task.attachments ?? []).find(
    (a) => a.type === "ElasticNetworkInterface",
  )
  const containers = task.containers ?? []

  return (
    <>
      <BackLink params={{ clusterName }} to="/ecs/list-clusters/$clusterName">
        {clusterName}
      </BackLink>

      <PageHeading
        actions={<StateBadge state={task.lastStatus} />}
        subtitle="Task Details"
        title={shortArn(task.taskArn)}
      />

      <DetailCard>
        <DetailCard.Header>Configuration</DetailCard.Header>
        <DetailCard.Content>
          <DetailRow label="Task ID" value={shortArn(task.taskArn)} />
          <DetailRow
            label="Last status"
            value={<StateBadge state={task.lastStatus} />}
          />
          <DetailRow label="Desired status" value={task.desiredStatus} />
          <DetailRow
            label="Task definition"
            value={shortArn(task.taskDefinitionArn)}
          />
          <DetailRow label="Launch type" value={task.launchType} />
          <DetailRow
            label="Container instance"
            value={shortArn(task.containerInstanceArn)}
          />
          <DetailRow label="Created at" value={formatTime(task.createdAt)} />
          <DetailRow label="Started at" value={formatTime(task.startedAt)} />
          <DetailRow label="Stopped at" value={formatTime(task.stoppedAt)} />
          {task.stoppedReason ? (
            <DetailRow label="Stopped reason" value={task.stoppedReason} />
          ) : null}
        </DetailCard.Content>
      </DetailCard>

      <div className="mt-6">
        <DetailCard>
          <DetailCard.Header>Networking</DetailCard.Header>
          {eni ? (
            <DetailCard.Content>
              <DetailRow
                label="ENI ID"
                value={attachmentDetail(eni, "networkInterfaceId")}
              />
              <DetailRow
                label="Private IP"
                value={attachmentDetail(eni, "privateIPv4Address")}
              />
              <DetailRow
                label="Public IP"
                value={attachmentDetail(eni, "publicIPv4Address")}
              />
              <DetailRow
                label="Subnet"
                value={attachmentDetail(eni, "subnetId")}
              />
              <DetailRow
                label="MAC"
                value={attachmentDetail(eni, "macAddress")}
              />
            </DetailCard.Content>
          ) : (
            <div className="p-4">
              <p className="text-muted-foreground">
                Network mode does not assign an ENI.
              </p>
            </div>
          )}
        </DetailCard>
      </div>

      <div className="mt-6">
        <h2 className="mb-2 font-semibold">Containers</h2>
        {containers.length === 0 ? (
          <p className="text-muted-foreground">No containers.</p>
        ) : (
          <div className="overflow-x-auto rounded-lg border bg-card">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left text-muted-foreground">
                  <th className="px-4 py-2 font-medium">Name</th>
                  <th className="px-4 py-2 font-medium">Image</th>
                  <th className="px-4 py-2 font-medium">Status</th>
                  <th className="px-4 py-2 font-medium">Health</th>
                  <th className="px-4 py-2 font-medium">Exit Code / Reason</th>
                </tr>
              </thead>
              <tbody>
                {containers.map((container: Container) => (
                  <tr
                    className="border-b last:border-0"
                    key={container.name ?? container.containerArn}
                  >
                    <td className="px-4 py-2 font-medium">{container.name}</td>
                    <td className="px-4 py-2 font-mono text-xs">
                      {container.image}
                    </td>
                    <td className="px-4 py-2">
                      <StateBadge state={container.lastStatus} />
                    </td>
                    <td className="px-4 py-2">{container.healthStatus}</td>
                    <td className="px-4 py-2">
                      {container.exitCode === undefined
                        ? (container.reason ?? "")
                        : String(container.exitCode)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  )
}
