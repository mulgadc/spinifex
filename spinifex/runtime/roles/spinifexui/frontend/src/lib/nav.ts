import type { LinkProps } from "@tanstack/react-router"
import {
  Activity,
  Boxes,
  Brain,
  Camera,
  Container,
  Crosshair,
  Database,
  Earth,
  FileStack,
  Globe,
  HardDrive,
  Home,
  IdCard,
  Image,
  Key,
  KeyRound,
  Layers,
  LayoutGrid,
  LayoutList,
  MapPin,
  MessageSquare,
  Network,
  Package,
  Route,
  Router,
  Server,
  Shield,
  ShieldCheck,
  SlidersHorizontal,
  UserCog,
  Users,
  UsersRound,
  Waypoints,
} from "lucide-react"
import type { LucideIcon } from "lucide-react"

// One typed source for the sidebar, breadcrumbs and command palette.
export interface NavItem {
  admin?: boolean
  // Highlighted when the path equals `to`, rather than starting with a prefix.
  exact?: boolean
  icon: LucideIcon
  label: string
  // Path prefixes that highlight the item; defaults to `to`.
  match?: readonly string[]
  to: LinkProps["to"] & string
  tooltip?: string
  // The item's create flow, offered by the command palette.
  create?: { label: string; to: LinkProps["to"] & string }
}

export interface NavGroup {
  items: readonly NavItem[]
  label: string
  ochre?: boolean
}

export const NAV: readonly NavGroup[] = [
  {
    label: "General",
    items: [
      { label: "Dashboard", icon: Home, to: "/", exact: true },
      { label: "Nodes", icon: Server, to: "/nodes", admin: true },
    ],
  },
  {
    label: "EC2",
    items: [
      {
        label: "Instances",
        icon: Server,
        to: "/ec2/describe-instances",
        create: { label: "Launch instance", to: "/ec2/run-instances" },
        match: ["/ec2/describe-instances", "/ec2/run-instances"],
      },
      {
        label: "Launch Templates",
        icon: FileStack,
        to: "/ec2/describe-launch-templates",
        create: {
          label: "Create launch template",
          to: "/ec2/create-launch-template",
        },
        match: [
          "/ec2/describe-launch-templates",
          "/ec2/create-launch-template",
        ],
      },
      { label: "Images", icon: Image, to: "/ec2/describe-images" },
      {
        label: "Key Pairs",
        icon: Key,
        to: "/ec2/describe-key-pairs",
        create: { label: "Create key pair", to: "/ec2/create-key-pair" },
      },
      {
        label: "Elastic IPs",
        icon: MapPin,
        to: "/ec2/describe-addresses",
        create: { label: "Allocate Elastic IP", to: "/ec2/allocate-address" },
        match: ["/ec2/describe-addresses", "/ec2/allocate-address"],
      },
      {
        label: "Volumes",
        icon: HardDrive,
        to: "/ec2/describe-volumes",
        create: { label: "Create volume", to: "/ec2/create-volume" },
        match: [
          "/ec2/describe-volumes",
          "/ec2/create-volume",
          "/ec2/modify-volume",
        ],
      },
      {
        label: "Snapshots",
        icon: Camera,
        to: "/ec2/describe-snapshots",
        create: { label: "Create snapshot", to: "/ec2/create-snapshot" },
        match: ["/ec2/describe-snapshots", "/ec2/create-snapshot"],
      },
      {
        label: "Placement Groups",
        icon: Layers,
        to: "/ec2/describe-placement-groups",
        create: {
          label: "Create placement group",
          to: "/ec2/create-placement-group",
        },
        match: [
          "/ec2/describe-placement-groups",
          "/ec2/create-placement-group",
        ],
      },
      {
        label: "Load Balancers",
        icon: Waypoints,
        to: "/ec2/describe-load-balancers",
        create: {
          label: "Create load balancer",
          to: "/ec2/create-load-balancer",
        },
        match: ["/ec2/describe-load-balancers", "/ec2/create-load-balancer"],
      },
      {
        label: "Target Groups",
        icon: Crosshair,
        to: "/ec2/describe-target-groups",
        create: {
          label: "Create target group",
          to: "/ec2/create-target-group",
        },
        match: ["/ec2/describe-target-groups", "/ec2/create-target-group"],
      },
    ],
  },
  {
    label: "Networking",
    items: [
      {
        label: "VPCs",
        icon: Network,
        to: "/ec2/describe-vpcs",
        create: { label: "Create VPC", to: "/ec2/create-vpc" },
        match: ["/ec2/describe-vpcs", "/ec2/create-vpc"],
      },
      {
        label: "Subnets",
        icon: LayoutGrid,
        to: "/ec2/describe-subnets",
        create: { label: "Create subnet", to: "/ec2/create-subnet" },
        match: ["/ec2/describe-subnets", "/ec2/create-subnet"],
      },
      {
        label: "Security Groups",
        icon: ShieldCheck,
        to: "/ec2/describe-security-groups",
        create: {
          label: "Create security group",
          to: "/ec2/create-security-group",
        },
        match: ["/ec2/describe-security-groups", "/ec2/create-security-group"],
      },
      {
        label: "Route Tables",
        icon: Route,
        to: "/ec2/describe-route-tables",
        create: { label: "Create route table", to: "/ec2/create-route-table" },
        match: ["/ec2/describe-route-tables", "/ec2/create-route-table"],
      },
      {
        label: "Internet Gateways",
        icon: Globe,
        to: "/ec2/describe-internet-gateways",
        create: {
          label: "Create internet gateway",
          to: "/ec2/create-internet-gateway",
        },
      },
      {
        label: "NAT Gateways",
        icon: Router,
        to: "/ec2/describe-nat-gateways",
        create: { label: "Create NAT gateway", to: "/ec2/create-nat-gateway" },
        match: ["/ec2/describe-nat-gateways", "/ec2/create-nat-gateway"],
      },
    ],
  },
  {
    label: "Containers",
    items: [
      {
        label: "EKS",
        tooltip: "EKS clusters",
        icon: Boxes,
        to: "/eks/list-clusters",
        create: { label: "Create EKS cluster", to: "/eks/create-cluster" },
        match: ["/eks/list-clusters", "/eks/create-cluster"],
      },
      {
        label: "ECR",
        tooltip: "ECR repositories",
        icon: Package,
        to: "/ecr/list-repositories",
      },
      {
        label: "ECS",
        tooltip: "ECS clusters",
        icon: Container,
        to: "/ecs/list-clusters",
        match: ["/ecs/"],
      },
    ],
  },
  {
    label: "RDS",
    items: [
      {
        label: "Databases",
        icon: Database,
        to: "/rds/describe-db-instances",
        create: { label: "Create database", to: "/rds/create-db-instance" },
        match: ["/rds/describe-db-instances", "/rds/create-db-instance"],
      },
      {
        label: "Snapshots",
        tooltip: "DB Snapshots",
        icon: Camera,
        to: "/rds/describe-db-snapshots",
        match: [
          "/rds/describe-db-snapshots",
          "/rds/restore-db-instance-from-db-snapshot",
        ],
      },
      {
        label: "Subnet Groups",
        tooltip: "DB Subnet Groups",
        icon: LayoutGrid,
        to: "/rds/describe-db-subnet-groups",
        create: {
          label: "Create DB subnet group",
          to: "/rds/create-db-subnet-group",
        },
        match: [
          "/rds/describe-db-subnet-groups",
          "/rds/create-db-subnet-group",
        ],
      },
      {
        label: "Parameter Groups",
        tooltip: "DB Parameter Groups",
        icon: SlidersHorizontal,
        to: "/rds/describe-db-parameter-groups",
        create: {
          label: "Create DB parameter group",
          to: "/rds/create-db-parameter-group",
        },
        match: [
          "/rds/describe-db-parameter-groups",
          "/rds/create-db-parameter-group",
        ],
      },
    ],
  },
  {
    label: "DNS",
    items: [
      {
        label: "Hosted zones",
        icon: Earth,
        to: "/route53/list-hosted-zones",
        create: {
          label: "Create hosted zone",
          to: "/route53/create-hosted-zone",
        },
        match: ["/route53/list-hosted-zones", "/route53/create-hosted-zone"],
      },
    ],
  },
  {
    label: "IAM",
    items: [
      {
        label: "Users",
        icon: Users,
        to: "/iam/list-users",
        create: { label: "Create user", to: "/iam/create-user" },
        match: ["/iam/list-users", "/iam/create-user"],
      },
      {
        label: "Groups",
        icon: UsersRound,
        to: "/iam/list-groups",
        create: { label: "Create group", to: "/iam/create-group" },
        match: ["/iam/list-groups", "/iam/create-group"],
      },
      {
        label: "Policies",
        icon: Shield,
        to: "/iam/list-policies",
        create: { label: "Create policy", to: "/iam/create-policy" },
        match: ["/iam/list-policies", "/iam/create-policy"],
      },
      {
        label: "Roles",
        icon: UserCog,
        to: "/iam/list-roles",
        create: { label: "Create role", to: "/iam/create-role" },
        match: ["/iam/list-roles", "/iam/create-role"],
      },
      {
        label: "Instance Profiles",
        icon: IdCard,
        to: "/iam/list-instance-profiles",
        create: {
          label: "Create instance profile",
          to: "/iam/create-instance-profile",
        },
        match: ["/iam/list-instance-profiles", "/iam/create-instance-profile"],
      },
    ],
  },
  {
    label: "S3",
    items: [
      {
        label: "Buckets",
        icon: Server,
        to: "/s3/ls",
        create: { label: "Create bucket", to: "/s3/create-bucket" },
      },
      {
        label: "Service Metrics",
        icon: Activity,
        to: "/s3/service-metrics",
        admin: true,
      },
    ],
  },
  {
    label: "Ochre",
    ochre: true,
    items: [
      {
        label: "Knowledge Bases",
        icon: Brain,
        to: "/bedrock/list-knowledge-bases",
        create: {
          label: "Create knowledge base",
          to: "/bedrock/list-knowledge-bases/create",
        },
      },
      {
        label: "Guardrails",
        icon: Shield,
        to: "/bedrock/list-guardrails",
        create: {
          label: "Create guardrail",
          to: "/bedrock/list-guardrails/create",
        },
      },
      { label: "Playground", icon: MessageSquare, to: "/bedrock/playground" },
      {
        label: "Model Catalog",
        icon: LayoutList,
        to: "/bedrock/list-model-catalog",
        admin: true,
      },
      {
        label: "Model Access",
        icon: KeyRound,
        to: "/bedrock/list-model-access",
        admin: true,
      },
    ],
  },
]

// The item a path belongs to. The longest matching prefix wins, so a nested
// route never lights up a shorter sibling as well.
export function activeNavItem(
  groups: readonly NavGroup[],
  pathname: string,
): { group: NavGroup; item: NavItem } | undefined {
  let best: { group: NavGroup; item: NavItem; length: number } | undefined

  for (const group of groups) {
    for (const item of group.items) {
      const length = matchLength(item, pathname)

      if (length > (best?.length ?? 0)) {
        best = { group, item, length }
      }
    }
  }

  return best && { group: best.group, item: best.item }
}

function matchLength(item: NavItem, pathname: string): number {
  if (item.exact) {
    return pathname === item.to ? item.to.length : 0
  }

  return Math.max(
    0,
    ...(item.match ?? [item.to])
      .filter((prefix) => isPathPrefix(prefix, pathname))
      .map((prefix) => prefix.length),
  )
}

// A prefix matches whole segments only: /iam/list-roles is not /iam/list-r.
function isPathPrefix(prefix: string, pathname: string): boolean {
  return (
    pathname === prefix ||
    pathname.startsWith(prefix.endsWith("/") ? prefix : `${prefix}/`)
  )
}

// The groups and items a caller may see.
export function visibleNav({
  isAdmin,
  ochreEnabled,
}: {
  isAdmin: boolean
  ochreEnabled: boolean
}): NavGroup[] {
  return NAV.flatMap((group) => {
    if (group.ochre && !ochreEnabled) {
      return []
    }

    const items = group.items.filter((item) => !item.admin || isAdmin)

    return items.length > 0 ? [{ ...group, items }] : []
  })
}

export interface Crumb {
  label: string
  // Absent on the current page and on a group, which has no page of its own.
  to?: string
}

// Service, then the nav item, then whatever the path adds below it: a create
// flow's own label, or the resource ID a detail route carries.
export function breadcrumbTrail(
  groups: readonly NavGroup[],
  pathname: string,
): Crumb[] {
  const active = activeNavItem(groups, pathname)

  if (!active) {
    return []
  }

  const { group, item } = active
  const trail: Crumb[] = [{ label: group.label }]

  if (pathname === item.to) {
    return [...trail, { label: item.label }]
  }

  trail.push({ label: item.label, to: item.to })

  if (item.create && pathname === item.create.to) {
    return [...trail, { label: item.create.label }]
  }

  const rest = pathname.startsWith(`${item.to}/`)
    ? pathname.slice(item.to.length + 1)
    : (pathname.split("/").at(-1) ?? "")

  const last = rest.split("/").findLast(Boolean)

  return last ? [...trail, { label: decodeSegment(last) }] : trail
}

function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment)
  } catch {
    return segment
  }
}
