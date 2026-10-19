import { Badge } from "@/components/ui/badge"
import type { NodeInfo } from "@/queries/admin"

function formatUptime(seconds: number): string {
  const days = Math.floor(seconds / 86_400)
  const hours = Math.floor((seconds % 86_400) / 3600)

  if (days > 0) {
    return `${days}d ${hours}h`
  }

  const minutes = Math.floor((seconds % 3600) / 60)

  return hours > 0 ? `${hours}h ${minutes}m` : `${minutes}m`
}

// Mirrors `spx get nodes`. Compact shows a service count where the full table
// lists the services.
export function NodesTable({
  nodes,
  compact = false,
}: {
  nodes: NodeInfo[]
  compact?: boolean
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="pr-4 pb-1 font-medium">Name</th>
            <th className="pr-4 pb-1 font-medium">Status</th>
            <th className="pr-4 pb-1 font-medium">Roles</th>
            <th className="pr-4 pb-1 font-medium">IP</th>
            <th className="pr-4 pb-1 font-medium">Region</th>
            <th className="pr-4 pb-1 font-medium">AZ</th>
            <th className="pr-4 pb-1 font-medium">Uptime</th>
            <th className="pr-4 pb-1 font-medium">EC2</th>
            <th className="pb-1 font-medium">Services</th>
          </tr>
        </thead>
        <tbody>
          {nodes.map((node) => {
            const roles = [
              node.nats_role ? `nats:${node.nats_role}` : null,
              node.predastore_role
                ? `predastore:${node.predastore_role}`
                : null,
            ]
              .filter(Boolean)
              .join(",")

            return (
              <tr key={node.node} className="border-b last:border-0">
                <td className="py-1.5 pr-4 font-mono font-medium">
                  {node.node}
                </td>
                <td className="py-1.5 pr-4">
                  <Badge
                    variant={node.status === "Ready" ? "default" : "secondary"}
                  >
                    {node.status}
                  </Badge>
                </td>
                <td className="py-1.5 pr-4 font-mono text-muted-foreground">
                  {roles || "-"}
                </td>
                <td className="py-1.5 pr-4 font-mono">{node.host}</td>
                <td className="py-1.5 pr-4">{node.region}</td>
                <td className="py-1.5 pr-4">{node.az}</td>
                <td className="py-1.5 pr-4">{formatUptime(node.uptime)}</td>
                <td className="py-1.5 pr-4">{node.vm_count}</td>
                <td className="py-1.5">
                  <span className="text-muted-foreground">
                    {compact ? node.services.length : node.services.join(",")}
                  </span>
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}
