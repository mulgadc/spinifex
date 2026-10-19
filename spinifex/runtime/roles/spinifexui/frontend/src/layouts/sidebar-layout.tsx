import { useQueryClient } from "@tanstack/react-query"
import { Link, useLocation, useNavigate } from "@tanstack/react-router"
import type { LinkProps } from "@tanstack/react-router"
import {
  Activity,
  BookOpen,
  Boxes,
  Brain,
  Camera,
  Container,
  Crosshair,
  Database,
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
  LogOut,
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

import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
  SidebarSeparator,
} from "@/components/ui/sidebar"
import { useAdmin } from "@/hooks/use-admin"
import { isOchreEnabled } from "@/lib/cluster-config"
import { signOut } from "@/lib/query-client"

interface NavItem {
  admin?: boolean
  // Highlighted when the path equals `to`, rather than starting with a prefix.
  exact?: boolean
  icon: LucideIcon
  label: string
  // Path prefixes that highlight the item; defaults to `to`.
  match?: readonly string[]
  to: LinkProps["to"] & string
  tooltip?: string
}

interface NavGroup {
  items: readonly NavItem[]
  label: string
  ochre?: boolean
}

const NAV: readonly NavGroup[] = [
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
        match: ["/ec2/describe-instances", "/ec2/run-instances"],
      },
      {
        label: "Launch Templates",
        icon: FileStack,
        to: "/ec2/describe-launch-templates",
        match: [
          "/ec2/describe-launch-templates",
          "/ec2/create-launch-template",
        ],
      },
      { label: "Images", icon: Image, to: "/ec2/describe-images" },
      { label: "Key Pairs", icon: Key, to: "/ec2/describe-key-pairs" },
      {
        label: "Elastic IPs",
        icon: MapPin,
        to: "/ec2/describe-addresses",
        match: ["/ec2/describe-addresses", "/ec2/allocate-address"],
      },
      {
        label: "Volumes",
        icon: HardDrive,
        to: "/ec2/describe-volumes",
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
        match: ["/ec2/describe-snapshots", "/ec2/create-snapshot"],
      },
      {
        label: "Placement Groups",
        icon: Layers,
        to: "/ec2/describe-placement-groups",
        match: [
          "/ec2/describe-placement-groups",
          "/ec2/create-placement-group",
        ],
      },
      {
        label: "Load Balancers",
        icon: Waypoints,
        to: "/ec2/describe-load-balancers",
        match: ["/ec2/describe-load-balancers", "/ec2/create-load-balancer"],
      },
      {
        label: "Target Groups",
        icon: Crosshair,
        to: "/ec2/describe-target-groups",
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
        match: ["/ec2/describe-vpcs", "/ec2/create-vpc"],
      },
      {
        label: "Subnets",
        icon: LayoutGrid,
        to: "/ec2/describe-subnets",
        match: ["/ec2/describe-subnets", "/ec2/create-subnet"],
      },
      {
        label: "Security Groups",
        icon: ShieldCheck,
        to: "/ec2/describe-security-groups",
        match: ["/ec2/describe-security-groups", "/ec2/create-security-group"],
      },
      {
        label: "Route Tables",
        icon: Route,
        to: "/ec2/describe-route-tables",
        match: ["/ec2/describe-route-tables", "/ec2/create-route-table"],
      },
      {
        label: "Internet Gateways",
        icon: Globe,
        to: "/ec2/describe-internet-gateways",
      },
      {
        label: "NAT Gateways",
        icon: Router,
        to: "/ec2/describe-nat-gateways",
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
        match: [
          "/rds/describe-db-parameter-groups",
          "/rds/create-db-parameter-group",
        ],
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
        match: ["/iam/list-users", "/iam/create-user"],
      },
      {
        label: "Groups",
        icon: UsersRound,
        to: "/iam/list-groups",
        match: ["/iam/list-groups", "/iam/create-group"],
      },
      {
        label: "Policies",
        icon: Shield,
        to: "/iam/list-policies",
        match: ["/iam/list-policies", "/iam/create-policy"],
      },
      {
        label: "Roles",
        icon: UserCog,
        to: "/iam/list-roles",
        match: ["/iam/list-roles", "/iam/create-role"],
      },
      {
        label: "Instance Profiles",
        icon: IdCard,
        to: "/iam/list-instance-profiles",
        match: ["/iam/list-instance-profiles", "/iam/create-instance-profile"],
      },
    ],
  },
  {
    label: "S3",
    items: [
      { label: "Buckets", icon: Server, to: "/s3/ls" },
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
      },
      { label: "Guardrails", icon: Shield, to: "/bedrock/list-guardrails" },
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

function isItemActive(item: NavItem, pathname: string): boolean {
  if (item.exact) {
    return pathname === item.to
  }

  return (item.match ?? [item.to]).some((prefix) => pathname.startsWith(prefix))
}

export function SidebarLayout() {
  const pathname = useLocation({
    select: (location) => location.pathname,
  })

  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { isAdmin } = useAdmin()
  const ochreEnabled = isOchreEnabled()

  function handleLogout() {
    signOut(queryClient)
    navigate({ to: "/login" })
  }

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader variant="brand">
        <img
          src="/mulga-logo.svg"
          alt="Mulga"
          className="size-6 shrink-0 dark:invert"
        />
        <span className="truncate text-sm font-semibold group-data-[collapsible=icon]:hidden">
          Spinifex
        </span>
      </SidebarHeader>
      <SidebarContent>
        {NAV.map((group) =>
          group.ochre && !ochreEnabled ? null : (
            <SidebarGroup key={group.label}>
              <SidebarGroupLabel>{group.label}</SidebarGroupLabel>
              <SidebarMenu>
                {group.items.map((item) =>
                  item.admin && !isAdmin ? null : (
                    <SidebarMenuItem key={item.to}>
                      <SidebarMenuButton
                        isActive={isItemActive(item, pathname)}
                        render={<Link to={item.to} />}
                        tooltip={item.tooltip ?? item.label}
                      >
                        <item.icon className="size-4" />
                        <span>{item.label}</span>
                      </SidebarMenuButton>
                    </SidebarMenuItem>
                  ),
                )}
              </SidebarMenu>
            </SidebarGroup>
          ),
        )}
        {isAdmin && (
          <SidebarGroup>
            <SidebarGroupLabel>Documentation</SidebarGroupLabel>
            <SidebarMenu>
              <SidebarMenuItem>
                <SidebarMenuButton
                  render={
                    // oxlint-disable-next-line jsx-a11y/control-has-associated-label -- SidebarMenuButton supplies the label
                    <a
                      href="https://docs.mulgadc.com"
                      rel="noopener noreferrer"
                      target="_blank"
                    />
                  }
                  tooltip="Documentation"
                >
                  <BookOpen className="size-4" />
                  <span>Docs</span>
                </SidebarMenuButton>
              </SidebarMenuItem>
            </SidebarMenu>
          </SidebarGroup>
        )}
      </SidebarContent>

      <SidebarFooter>
        <SidebarSeparator className="mx-0 w-full" />
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton onClick={handleLogout} tooltip="Sign out">
              <LogOut className="size-4" />
              <span>Sign out</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>

      <SidebarRail />
    </Sidebar>
  )
}
