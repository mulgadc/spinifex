import type { Service } from "@aws-sdk/client-ecs"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"

import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { ecsServicesQueryOptions } from "@/queries/ecs"

// shortArn renders the trailing resource segment of an ARN (the part after the
// final "/"), falling back to the whole string when there is no slash.
function shortArn(arn: string | undefined): string {
  if (!arn) {
    return ""
  }
  const idx = arn.lastIndexOf("/")
  return idx === -1 ? arn : arn.slice(idx + 1)
}

export function ServicesTab({ clusterName }: { clusterName: string }) {
  const { data: services } = useSuspenseQuery(
    ecsServicesQueryOptions(clusterName),
  )

  return (
    <>
      <div className="mb-4 flex justify-end">
        <Link search={{ cluster: clusterName }} to="/ecs/create-service">
          <Button size="sm">Create service</Button>
        </Link>
      </div>

      {services.length === 0 ? (
        <p className="text-muted-foreground">No services found.</p>
      ) : (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 font-medium">Status</th>
                <th className="px-4 py-2 font-medium">Desired</th>
                <th className="px-4 py-2 font-medium">Running</th>
                <th className="px-4 py-2 font-medium">Pending</th>
                <th className="px-4 py-2 font-medium">Task Definition</th>
              </tr>
            </thead>
            <tbody>
              {services.map((service: Service) => (
                <tr
                  className="border-b last:border-0"
                  key={service.serviceArn ?? service.serviceName}
                >
                  <td className="px-4 py-2 font-medium">
                    {service.serviceName}
                  </td>
                  <td className="px-4 py-2">
                    <StateBadge state={service.status} />
                  </td>
                  <td className="px-4 py-2">{service.desiredCount ?? 0}</td>
                  <td className="px-4 py-2">{service.runningCount ?? 0}</td>
                  <td className="px-4 py-2">{service.pendingCount ?? 0}</td>
                  <td className="px-4 py-2 font-mono text-xs">
                    {shortArn(service.taskDefinition)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}
