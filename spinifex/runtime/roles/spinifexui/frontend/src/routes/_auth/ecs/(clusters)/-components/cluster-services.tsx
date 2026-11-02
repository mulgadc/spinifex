import type { Service } from "@aws-sdk/client-ecs"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"

import { PanelSection } from "@/components/detail-panel"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { arnResourceId } from "@/lib/utils"
import { ecsServicesQueryOptions } from "@/queries/ecs"

import { PanelList, PanelListItem } from "./panel-list"

export function ClusterServices({ clusterName }: { clusterName: string }) {
  const { data: services } = useSuspenseQuery(
    ecsServicesQueryOptions(clusterName),
  )

  return (
    <PanelSection title={`Services (${services.length})`}>
      <div className="flex flex-col items-start gap-3">
        {services.length === 0 ? (
          <p className="text-sm text-muted-foreground">No services found.</p>
        ) : (
          <div className="w-full">
            <PanelList>
              {services.map((service: Service) => (
                <PanelListItem
                  detail={`${service.runningCount ?? 0} running of ${service.desiredCount ?? 0} desired, ${service.pendingCount ?? 0} pending · ${arnResourceId(service.taskDefinition)}`}
                  key={service.serviceArn ?? service.serviceName}
                  state={<StateBadge state={service.status} />}
                  title={
                    <Link
                      className="font-medium text-primary hover:underline"
                      params={{
                        clusterName,
                        serviceName: service.serviceName ?? "",
                      }}
                      to="/ecs/list-clusters/$clusterName/services/$serviceName"
                    >
                      {service.serviceName}
                    </Link>
                  }
                />
              ))}
            </PanelList>
          </div>
        )}
        <Link
          className={buttonVariants({ size: "sm", variant: "outline" })}
          search={{ selected: clusterName, launch: "service" }}
          to="/ecs/list-clusters"
        >
          <Plus data-icon="inline-start" />
          Create service
        </Link>
      </div>
    </PanelSection>
  )
}
