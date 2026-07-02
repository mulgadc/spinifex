import { Link } from "@tanstack/react-router"

const BASE = "-mb-px border-b-2 px-3 py-2 text-sm font-medium transition-colors"
const ACTIVE = { className: "border-primary text-foreground" }
const INACTIVE = {
  className: "border-transparent text-muted-foreground hover:text-foreground",
}

// EcsSectionNav mirrors the AWS ECS console's top-level sections (Clusters and
// Task definitions are account-scoped siblings, not nested under a cluster).
export function EcsSectionNav() {
  return (
    <nav className="mb-6 flex gap-1 border-b">
      <Link
        activeOptions={{ exact: false }}
        activeProps={ACTIVE}
        className={BASE}
        inactiveProps={INACTIVE}
        to="/ecs/list-clusters"
      >
        Clusters
      </Link>
      <Link
        activeOptions={{ exact: false }}
        activeProps={ACTIVE}
        className={BASE}
        inactiveProps={INACTIVE}
        to="/ecs/task-definitions"
      >
        Task definitions
      </Link>
    </nav>
  )
}
