import type {
  Deployment,
  LoadBalancer,
  ServiceEvent,
  Task,
} from "@aws-sdk/client-ecs"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"

import { BackLink } from "@/components/back-link"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { ecsServicesQueryOptions, ecsTasksQueryOptions } from "@/queries/ecs"

// shortArn renders the trailing resource segment of an ARN, falling back to the
// whole string when there is no slash.
function shortArn(arn: string | undefined): string {
  if (!arn) {
    return ""
  }
  const idx = arn.lastIndexOf("/")
  return idx === -1 ? arn : arn.slice(idx + 1)
}

function attachmentDetail(task: Task, name: string): string | undefined {
  const eni = (task.attachments ?? []).find(
    (a) => a.type === "ElasticNetworkInterface",
  )
  return eni?.details?.find((d) => d.name === name)?.value
}

export function ServiceDetailPage({
  clusterName,
  serviceName,
}: {
  clusterName: string
  serviceName: string
}) {
  const { data: services } = useSuspenseQuery(
    ecsServicesQueryOptions(clusterName),
  )
  const { data: tasks } = useSuspenseQuery(ecsTasksQueryOptions(clusterName))
  const service = services.find((s) => s.serviceName === serviceName)

  if (!service) {
    return (
      <>
        <BackLink params={{ clusterName }} to="/ecs/list-clusters/$clusterName">
          {clusterName}
        </BackLink>
        <PageHeading subtitle="Service Details" title={serviceName} />
        <p className="text-muted-foreground">Service not found.</p>
      </>
    )
  }

  const loadBalancers = service.loadBalancers ?? []
  const memberTasks = tasks.filter(
    (t: Task) => t.group === `service:${serviceName}`,
  )
  const deployments = service.deployments ?? []
  const events = service.events ?? []

  return (
    <>
      <BackLink params={{ clusterName }} to="/ecs/list-clusters/$clusterName">
        {clusterName}
      </BackLink>

      <PageHeading
        actions={<StateBadge state={service.status} />}
        subtitle="Service Details"
        title={serviceName}
      />

      <DetailCard>
        <DetailCard.Header>Configuration</DetailCard.Header>
        <DetailCard.Content>
          <DetailRow
            label="Status"
            value={<StateBadge state={service.status} />}
          />
          <DetailRow
            label="Desired count"
            value={String(service.desiredCount ?? 0)}
          />
          <DetailRow
            label="Running count"
            value={String(service.runningCount ?? 0)}
          />
          <DetailRow
            label="Pending count"
            value={String(service.pendingCount ?? 0)}
          />
          <DetailRow
            label="Task definition"
            value={shortArn(service.taskDefinition)}
          />
          <DetailRow label="Launch type" value={service.launchType} />
          <DetailRow
            label="Scheduling strategy"
            value={service.schedulingStrategy}
          />
        </DetailCard.Content>
      </DetailCard>

      <div className="mt-6">
        <h2 className="mb-2 font-semibold">Load balancers</h2>
        {loadBalancers.length === 0 ? (
          <p className="text-muted-foreground">None</p>
        ) : (
          <div className="overflow-x-auto rounded-lg border bg-card">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left text-muted-foreground">
                  <th className="px-4 py-2 font-medium">Target Group</th>
                  <th className="px-4 py-2 font-medium">Container</th>
                  <th className="px-4 py-2 font-medium">Port</th>
                </tr>
              </thead>
              <tbody>
                {loadBalancers.map((lb: LoadBalancer) => (
                  <tr
                    className="border-b last:border-0"
                    key={`${lb.targetGroupArn ?? ""}-${lb.containerName ?? ""}-${lb.containerPort ?? ""}`}
                  >
                    <td className="px-4 py-2 font-mono text-xs">
                      {shortArn(lb.targetGroupArn)}
                    </td>
                    <td className="px-4 py-2">{lb.containerName}</td>
                    <td className="px-4 py-2">{lb.containerPort}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="mt-6">
        <h2 className="mb-2 font-semibold">Tasks</h2>
        {memberTasks.length === 0 ? (
          <p className="text-muted-foreground">No tasks found.</p>
        ) : (
          <div className="overflow-x-auto rounded-lg border bg-card">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left text-muted-foreground">
                  <th className="px-4 py-2 font-medium">Task</th>
                  <th className="px-4 py-2 font-medium">Last Status</th>
                  <th className="px-4 py-2 font-medium">Private IP</th>
                  <th className="px-4 py-2 font-medium">Public IP</th>
                </tr>
              </thead>
              <tbody>
                {memberTasks.map((task: Task) => {
                  const id = shortArn(task.taskArn)
                  return (
                    <tr className="border-b last:border-0" key={id}>
                      <td className="px-4 py-2 font-mono text-xs">
                        <Link
                          className="underline"
                          params={{ clusterName, taskId: id }}
                          to="/ecs/list-clusters/$clusterName/tasks/$taskId"
                        >
                          {id}
                        </Link>
                      </td>
                      <td className="px-4 py-2">
                        <StateBadge state={task.lastStatus} />
                      </td>
                      <td className="px-4 py-2 font-mono text-xs">
                        {attachmentDetail(task, "privateIPv4Address") ?? "—"}
                      </td>
                      <td className="px-4 py-2 font-mono text-xs">
                        {attachmentDetail(task, "publicIPv4Address") ?? "—"}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="mt-6">
        <h2 className="mb-2 font-semibold">Deployments</h2>
        {deployments.length === 0 ? (
          <p className="text-muted-foreground">None</p>
        ) : (
          <div className="overflow-x-auto rounded-lg border bg-card">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left text-muted-foreground">
                  <th className="px-4 py-2 font-medium">Status</th>
                  <th className="px-4 py-2 font-medium">Task Definition</th>
                  <th className="px-4 py-2 font-medium">Desired</th>
                  <th className="px-4 py-2 font-medium">Running</th>
                  <th className="px-4 py-2 font-medium">Pending</th>
                </tr>
              </thead>
              <tbody>
                {deployments.map((deployment: Deployment) => (
                  <tr className="border-b last:border-0" key={deployment.id}>
                    <td className="px-4 py-2">{deployment.status}</td>
                    <td className="px-4 py-2 font-mono text-xs">
                      {shortArn(deployment.taskDefinition)}
                    </td>
                    <td className="px-4 py-2">
                      {deployment.desiredCount ?? 0}
                    </td>
                    <td className="px-4 py-2">
                      {deployment.runningCount ?? 0}
                    </td>
                    <td className="px-4 py-2">
                      {deployment.pendingCount ?? 0}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="mt-6">
        <h2 className="mb-2 font-semibold">Events</h2>
        {events.length === 0 ? (
          <p className="text-muted-foreground">None</p>
        ) : (
          <div className="overflow-x-auto rounded-lg border bg-card">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left text-muted-foreground">
                  <th className="px-4 py-2 font-medium">Time</th>
                  <th className="px-4 py-2 font-medium">Message</th>
                </tr>
              </thead>
              <tbody>
                {events.map((event: ServiceEvent) => (
                  <tr className="border-b last:border-0" key={event.id}>
                    <td className="px-4 py-2 font-mono text-xs">
                      {event.createdAt
                        ? new Date(event.createdAt).toISOString()
                        : ""}
                    </td>
                    <td className="px-4 py-2">{event.message}</td>
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
