import type {
  Deployment,
  LoadBalancer,
  Service,
  ServiceEvent,
  Task,
} from "@aws-sdk/client-ecs"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal } from "lucide-react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { arnResourceId, formatDateTime } from "@/lib/utils"

import { PanelList, PanelListItem } from "./panel-list"

function attachmentDetail(task: Task, name: string): string | undefined {
  const eni = (task.attachments ?? []).find(
    (a) => a.type === "ElasticNetworkInterface",
  )

  return eni?.details?.find((d) => d.name === name)?.value
}

function Overview({
  clusterName,
  service,
  tasks,
}: {
  clusterName: string
  service: Service
  tasks: readonly Task[]
}) {
  const loadBalancers = service.loadBalancers ?? []

  return (
    <>
      <PanelSection title="Configuration">
        <PanelFields>
          <PanelField copy={service.serviceArn} label="ARN" mono>
            {service.serviceArn}
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
          <PanelField label="Desired count">
            {service.desiredCount ?? 0}
          </PanelField>
          <PanelField label="Running count">
            {service.runningCount ?? 0}
          </PanelField>
          <PanelField label="Pending count">
            {service.pendingCount ?? 0}
          </PanelField>
          <PanelField label="Task definition" mono>
            {arnResourceId(service.taskDefinition)}
          </PanelField>
          <PanelField label="Launch type">{service.launchType}</PanelField>
          <PanelField label="Scheduling strategy">
            {service.schedulingStrategy}
          </PanelField>
        </PanelFields>
      </PanelSection>

      <PanelSection title={`Load balancers (${loadBalancers.length})`}>
        {loadBalancers.length === 0 ? (
          <p className="text-sm text-muted-foreground">None</p>
        ) : (
          <PanelList>
            {loadBalancers.map((lb: LoadBalancer) => (
              <PanelListItem
                detail={`Container ${lb.containerName ?? "—"}, port ${lb.containerPort ?? "—"}`}
                key={`${lb.targetGroupArn ?? ""}-${lb.containerName ?? ""}-${lb.containerPort ?? ""}`}
                title={
                  <span className="font-mono text-xs">
                    {arnResourceId(lb.targetGroupArn)}
                  </span>
                }
              />
            ))}
          </PanelList>
        )}
      </PanelSection>

      <PanelSection title={`Tasks (${tasks.length})`}>
        {tasks.length === 0 ? (
          <p className="text-sm text-muted-foreground">No tasks found.</p>
        ) : (
          <PanelList>
            {tasks.map((task) => {
              const id = arnResourceId(task.taskArn)

              return (
                <PanelListItem
                  detail={`Private ${attachmentDetail(task, "privateIPv4Address") ?? "—"} · Public ${attachmentDetail(task, "publicIPv4Address") ?? "—"}`}
                  key={id}
                  state={<StateBadge state={task.lastStatus} />}
                  title={
                    <Link
                      className="font-mono text-xs text-primary hover:underline"
                      params={{ clusterName, taskId: id }}
                      to="/ecs/list-clusters/$clusterName/tasks/$taskId"
                    >
                      {id}
                    </Link>
                  }
                />
              )
            })}
          </PanelList>
        )}
      </PanelSection>
    </>
  )
}

function Settings({ service }: { service: Service }) {
  const deployments = service.deployments ?? []
  const events = service.events ?? []

  return (
    <>
      <PanelSection title={`Deployments (${deployments.length})`}>
        {deployments.length === 0 ? (
          <p className="text-sm text-muted-foreground">None</p>
        ) : (
          <PanelList>
            {deployments.map((deployment: Deployment) => (
              <PanelListItem
                detail={`${deployment.runningCount ?? 0} running of ${deployment.desiredCount ?? 0} desired, ${deployment.pendingCount ?? 0} pending`}
                key={deployment.id}
                state={<StateBadge state={deployment.status} />}
                title={
                  <span className="font-mono text-xs">
                    {arnResourceId(deployment.taskDefinition)}
                  </span>
                }
              />
            ))}
          </PanelList>
        )}
      </PanelSection>

      <PanelSection title={`Events (${events.length})`}>
        {events.length === 0 ? (
          <p className="text-sm text-muted-foreground">None</p>
        ) : (
          <PanelList>
            {events.map((event: ServiceEvent) => (
              <li className="flex flex-col gap-0.5 py-2" key={event.id}>
                <span className="text-xs text-muted-foreground tabular-nums">
                  {formatDateTime(event.createdAt)}
                </span>
                <span className="break-words">{event.message}</span>
              </li>
            ))}
          </PanelList>
        )}
      </PanelSection>
    </>
  )
}

interface ServiceDetailsProps {
  clusterName: string
  service: Service
  // The cluster's tasks; the service's own are those in its group.
  tasks: readonly Task[]
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  onClose: () => void
  layout?: "auto" | "page"
}

// One ECS service's details, as a side panel or as its own page.
export function ServiceDetails({
  clusterName,
  service,
  tasks,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: ServiceDetailsProps) {
  const name = service.serviceName ?? ""
  const memberTasks = tasks.filter((t) => t.group === `service:${name}`)

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? `Back to ${clusterName}` : "Close service details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open service page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ clusterName, serviceName: name }}
            search={{ tab }}
            to="/ecs/list-clusters/$clusterName/services/$serviceName"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Service details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={clusterName}
      summary={<StateBadge state={service.status} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: (
            <Overview
              clusterName={clusterName}
              service={service}
              tasks={memberTasks}
            />
          ),
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings service={service} />,
        },
      ]}
      title={name}
    />
  )
}
