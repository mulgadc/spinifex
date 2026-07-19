import { useQueryClient } from "@tanstack/react-query"
import { Link, useLocation, useNavigate } from "@tanstack/react-router"
import {
  Activity,
  BookOpen,
  Boxes,
  Camera,
  Container,
  Crosshair,
  FileStack,
  Globe,
  HardDrive,
  Home,
  IdCard,
  Image,
  Key,
  Layers,
  LayoutGrid,
  LogOut,
  MapPin,
  Network,
  Package,
  Route,
  Router,
  Server,
  Shield,
  ShieldCheck,
  UserCog,
  Users,
  UsersRound,
  Waypoints,
} from "lucide-react"

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
import { useAdmin } from "@/contexts/admin-context"
import { clearCredentials } from "@/lib/auth"
import { clearClients } from "@/lib/awsClient"

export function SidebarLayout() {
  const pathname = useLocation({
    select: (location) => location.pathname,
  })
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { isAdmin } = useAdmin()

  function handleLogout() {
    clearCredentials()
    clearClients()
    queryClient.clear()
    navigate({ to: "/login" })
  }

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader className="flex flex-row items-center gap-2 px-4 py-3">
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
        <SidebarGroup>
          <SidebarGroupLabel>General</SidebarGroupLabel>
          <SidebarMenu>
            <SidebarMenuItem>
              <Link to="/">
                <SidebarMenuButton
                  isActive={pathname === "/"}
                  tooltip="Dashboard"
                >
                  <Home className="size-4" />
                  <span>Dashboard</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>
            {isAdmin && (
              <SidebarMenuItem>
                <Link to="/nodes">
                  <SidebarMenuButton
                    isActive={pathname.startsWith("/nodes")}
                    tooltip="Nodes"
                  >
                    <Server className="size-4" />
                    <span>Nodes</span>
                  </SidebarMenuButton>
                </Link>
              </SidebarMenuItem>
            )}
          </SidebarMenu>
        </SidebarGroup>

        <SidebarGroup>
          <SidebarGroupLabel>EC2</SidebarGroupLabel>
          <SidebarMenu>
            <SidebarMenuItem>
              <Link to="/ec2/describe-instances">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-instances") ||
                    pathname.startsWith("/ec2/run-instances")
                  }
                  tooltip="Instances"
                >
                  <Server className="size-4" />
                  <span>Instances</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-launch-templates">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-launch-templates") ||
                    pathname.startsWith("/ec2/create-launch-template")
                  }
                  tooltip="Launch Templates"
                >
                  <FileStack className="size-4" />
                  <span>Launch Templates</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-images">
                <SidebarMenuButton
                  isActive={pathname.startsWith("/ec2/describe-images")}
                  tooltip="Images"
                >
                  <Image className="size-4" />
                  <span>Images</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-key-pairs">
                <SidebarMenuButton
                  isActive={pathname.startsWith("/ec2/describe-key-pairs")}
                  tooltip="Key Pairs"
                >
                  <Key className="size-4" />
                  <span>Key Pairs</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-addresses">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-addresses") ||
                    pathname.startsWith("/ec2/allocate-address")
                  }
                  tooltip="Elastic IPs"
                >
                  <MapPin className="size-4" />
                  <span>Elastic IPs</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-volumes">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-volumes") ||
                    pathname.startsWith("/ec2/create-volume") ||
                    pathname.startsWith("/ec2/modify-volume")
                  }
                  tooltip="Volumes"
                >
                  <HardDrive className="size-4" />
                  <span>Volumes</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-snapshots">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-snapshots") ||
                    pathname.startsWith("/ec2/create-snapshot")
                  }
                  tooltip="Snapshots"
                >
                  <Camera className="size-4" />
                  <span>Snapshots</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-placement-groups">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-placement-groups") ||
                    pathname.startsWith("/ec2/create-placement-group")
                  }
                  tooltip="Placement Groups"
                >
                  <Layers className="size-4" />
                  <span>Placement Groups</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-load-balancers">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-load-balancers") ||
                    pathname.startsWith("/ec2/create-load-balancer")
                  }
                  tooltip="Load Balancers"
                >
                  <Waypoints className="size-4" />
                  <span>Load Balancers</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-target-groups">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-target-groups") ||
                    pathname.startsWith("/ec2/create-target-group")
                  }
                  tooltip="Target Groups"
                >
                  <Crosshair className="size-4" />
                  <span>Target Groups</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarGroup>

        <SidebarGroup>
          <SidebarGroupLabel>Networking</SidebarGroupLabel>
          <SidebarMenu>
            <SidebarMenuItem>
              <Link to="/ec2/describe-vpcs">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-vpcs") ||
                    pathname.startsWith("/ec2/create-vpc")
                  }
                  tooltip="VPCs"
                >
                  <Network className="size-4" />
                  <span>VPCs</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-subnets">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-subnets") ||
                    pathname.startsWith("/ec2/create-subnet")
                  }
                  tooltip="Subnets"
                >
                  <LayoutGrid className="size-4" />
                  <span>Subnets</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-security-groups">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-security-groups") ||
                    pathname.startsWith("/ec2/create-security-group")
                  }
                  tooltip="Security Groups"
                >
                  <ShieldCheck className="size-4" />
                  <span>Security Groups</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-route-tables">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-route-tables") ||
                    pathname.startsWith("/ec2/create-route-table")
                  }
                  tooltip="Route Tables"
                >
                  <Route className="size-4" />
                  <span>Route Tables</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-internet-gateways">
                <SidebarMenuButton
                  isActive={pathname.startsWith(
                    "/ec2/describe-internet-gateways",
                  )}
                  tooltip="Internet Gateways"
                >
                  <Globe className="size-4" />
                  <span>Internet Gateways</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/ec2/describe-nat-gateways">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/ec2/describe-nat-gateways") ||
                    pathname.startsWith("/ec2/create-nat-gateway")
                  }
                  tooltip="NAT Gateways"
                >
                  <Router className="size-4" />
                  <span>NAT Gateways</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarGroup>

        <SidebarGroup>
          <SidebarGroupLabel>Containers</SidebarGroupLabel>
          <SidebarMenu>
            <SidebarMenuItem>
              <Link to="/eks/list-clusters">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/eks/list-clusters") ||
                    pathname.startsWith("/eks/create-cluster")
                  }
                  tooltip="EKS clusters"
                >
                  <Boxes className="size-4" />
                  <span>EKS</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>
            <SidebarMenuItem>
              <Link to="/ecr/list-repositories">
                <SidebarMenuButton
                  isActive={pathname.startsWith("/ecr/list-repositories")}
                  tooltip="ECR repositories"
                >
                  <Package className="size-4" />
                  <span>ECR</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>
            <SidebarMenuItem>
              <Link to="/ecs/list-clusters">
                <SidebarMenuButton
                  isActive={pathname.startsWith("/ecs/")}
                  tooltip="ECS clusters"
                >
                  <Container className="size-4" />
                  <span>ECS</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarGroup>

        <SidebarGroup>
          <SidebarGroupLabel>IAM</SidebarGroupLabel>
          <SidebarMenu>
            <SidebarMenuItem>
              <Link to="/iam/list-users">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/iam/list-users") ||
                    pathname.startsWith("/iam/create-user")
                  }
                  tooltip="Users"
                >
                  <Users className="size-4" />
                  <span>Users</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/iam/list-groups">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/iam/list-groups") ||
                    pathname.startsWith("/iam/create-group")
                  }
                  tooltip="Groups"
                >
                  <UsersRound className="size-4" />
                  <span>Groups</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/iam/list-policies">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/iam/list-policies") ||
                    pathname.startsWith("/iam/create-policy")
                  }
                  tooltip="Policies"
                >
                  <Shield className="size-4" />
                  <span>Policies</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/iam/list-roles">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/iam/list-roles") ||
                    pathname.startsWith("/iam/create-role")
                  }
                  tooltip="Roles"
                >
                  <UserCog className="size-4" />
                  <span>Roles</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>

            <SidebarMenuItem>
              <Link to="/iam/list-instance-profiles">
                <SidebarMenuButton
                  isActive={
                    pathname.startsWith("/iam/list-instance-profiles") ||
                    pathname.startsWith("/iam/create-instance-profile")
                  }
                  tooltip="Instance Profiles"
                >
                  <IdCard className="size-4" />
                  <span>Instance Profiles</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarGroup>

        <SidebarGroup>
          <SidebarGroupLabel>S3</SidebarGroupLabel>
          <SidebarMenu>
            <SidebarMenuItem>
              <Link to="/s3/ls">
                <SidebarMenuButton
                  isActive={pathname.startsWith("/s3/ls")}
                  tooltip="Buckets"
                >
                  <Server className="size-4" />
                  <span>Buckets</span>
                </SidebarMenuButton>
              </Link>
            </SidebarMenuItem>
            {isAdmin && (
              <SidebarMenuItem>
                <Link to="/s3/service-metrics">
                  <SidebarMenuButton
                    isActive={pathname.startsWith("/s3/service-metrics")}
                    tooltip="Service Metrics"
                  >
                    <Activity className="size-4" />
                    <span>Service Metrics</span>
                  </SidebarMenuButton>
                </Link>
              </SidebarMenuItem>
            )}
          </SidebarMenu>
        </SidebarGroup>
        {isAdmin && (
          <SidebarGroup>
            <SidebarGroupLabel>Documentation</SidebarGroupLabel>
            <SidebarMenu>
              <SidebarMenuItem>
                <a
                  href="https://docs.mulgadc.com"
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  <SidebarMenuButton tooltip="Documentation">
                    <BookOpen className="size-4" />
                    <span>Docs</span>
                  </SidebarMenuButton>
                </a>
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
