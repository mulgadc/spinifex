import { Link } from "@tanstack/react-router"

const BASE = "-mb-px border-b-2 px-3 py-2 text-sm font-medium transition-colors"
const ACTIVE = { className: "border-primary text-foreground" }
const INACTIVE = {
  className: "border-transparent text-muted-foreground hover:text-foreground",
}

// Mirrors the AWS RDS console's top-level sections. Subnet groups and parameter
// groups join this nav when their pages land.
export function RdsSectionNav() {
  return (
    <nav className="mb-6 flex gap-1 border-b">
      <Link
        activeOptions={{ exact: false }}
        activeProps={ACTIVE}
        className={BASE}
        inactiveProps={INACTIVE}
        to="/rds/describe-db-instances"
      >
        Databases
      </Link>
    </nav>
  )
}
