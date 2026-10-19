import type { Vpc } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Link, createFileRoute } from "@tanstack/react-router"
import type { LinkProps } from "@tanstack/react-router"
import {
  Box,
  Globe,
  HardDrive,
  Key,
  Network,
  FolderOpen,
  Router,
  Shield,
  Waypoints,
} from "lucide-react"
import type { LucideIcon } from "lucide-react"
import type { ReactNode } from "react"

import { AdminDashboardPanel } from "@/components/admin-dashboard-panel"
import { PageHeading } from "@/components/page-heading"
import { SandboxWelcomeDialog } from "@/components/sandbox-welcome-dialog"
import { StateBadge } from "@/components/state-badge"
import {
  Card,
  CardAction,
  CardContent,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { getNameTag } from "@/lib/utils"
import {
  ec2AddressesQueryOptions,
  ec2InstancesQueryOptions,
  ec2InternetGatewaysQueryOptions,
  ec2KeyPairsQueryOptions,
  ec2NatGatewaysQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2VolumesQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import { s3BucketsQueryOptions } from "@/queries/s3"

const MAX_ITEMS = 5

export const Route = createFileRoute("/_auth/")({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2InstancesQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2VolumesQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2VpcsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2KeyPairsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...s3BucketsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2SecurityGroupsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2NatGatewaysQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2AddressesQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2InternetGatewaysQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: () => ({
    meta: [{ title: "Dashboard | Mulga" }],
  }),
  component: Dashboard,
})

function Dashboard() {
  const vpcs = useQuery(ec2VpcsQueryOptions).data?.Vpcs
  const keyPairs = useQuery(ec2KeyPairsQueryOptions).data?.KeyPairs
  const buckets = useQuery(s3BucketsQueryOptions).data?.Buckets
  const groups = useQuery(ec2SecurityGroupsQueryOptions).data?.SecurityGroups
  const natGateways = useQuery(ec2NatGatewaysQueryOptions).data?.NatGateways
  const addresses = useQuery(ec2AddressesQueryOptions).data?.Addresses
  const igws = useQuery(ec2InternetGatewaysQueryOptions).data?.InternetGateways

  return (
    <>
      <PageHeading title="Dashboard" />
      <SandboxWelcomeDialog />
      <AdminDashboardPanel />
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
        <InstancesCard />
        <VolumesCard />
        <ResourceCard
          emptyText="No VPCs"
          getKey={(vpc) => vpc.VpcId}
          icon={Network}
          items={vpcs}
          primary={vpcIdWithName}
          secondary={(vpc) => vpc.CidrBlock}
          title="VPCs"
          to="/ec2/describe-vpcs"
        />
        <ResourceCard
          emptyText="No key pairs"
          getKey={(kp) => kp.KeyPairId}
          icon={Key}
          items={keyPairs}
          primary={(kp) => kp.KeyName}
          title="Key Pairs"
          to="/ec2/describe-key-pairs"
        />
        <ResourceCard
          emptyText="No buckets"
          getKey={(b) => b.Name}
          icon={FolderOpen}
          items={buckets}
          primary={(b) => b.Name}
          title="S3 Buckets"
          to="/s3/ls"
        />
        <ResourceCard
          emptyText="No security groups"
          getKey={(sg) => sg.GroupId}
          icon={Shield}
          items={groups}
          primary={(sg) => sg.GroupName}
          secondary={(sg) => sg.VpcId}
          title="Security Groups"
          to="/ec2/describe-security-groups"
        />
        <ResourceCard
          emptyText="No NAT gateways"
          getKey={(nat) => nat.NatGatewayId}
          icon={Waypoints}
          items={natGateways}
          primary={(nat) => nat.NatGatewayId}
          secondary={(nat) => nat.State}
          title="NAT Gateways"
          to="/ec2/describe-nat-gateways"
        />
        <ResourceCard
          emptyText="No Elastic IPs"
          getKey={(addr) => addr.AllocationId}
          icon={Globe}
          items={addresses}
          primary={(addr) => addr.PublicIp}
          secondary={(addr) =>
            addr.AssociationId ? "associated" : "available"
          }
          title="Elastic IPs"
          to="/ec2/describe-addresses"
        />
        <ResourceCard
          emptyText="No internet gateways"
          getKey={(igw) => igw.InternetGatewayId}
          icon={Router}
          items={igws}
          primary={(igw) => igw.InternetGatewayId}
          secondary={(igw) =>
            igw.Attachments?.[0]?.VpcId ? "attached" : "detached"
          }
          title="Internet Gateways"
          to="/ec2/describe-internet-gateways"
        />
      </div>
    </>
  )
}

function vpcIdWithName(vpc: Vpc): ReactNode {
  const name = getNameTag(vpc.Tags)

  return (
    <>
      {vpc.VpcId}
      {name && (
        <span className="ml-1 font-sans text-muted-foreground">({name})</span>
      )}
    </>
  )
}

interface DashboardCardProps {
  title: string
  icon: ReactNode
  count: number
  to: LinkProps["to"]
  children: ReactNode
}

function DashboardCard({
  title,
  icon,
  count,
  to,
  children,
}: DashboardCardProps) {
  return (
    <Card>
      <CardHeader>
        <CardTitle variant="icon">
          {icon}
          {title}
        </CardTitle>
        <CardAction>
          <span className="rounded-md bg-muted px-2 py-0.5 text-xs font-semibold tabular-nums">
            {count}
          </span>
        </CardAction>
      </CardHeader>
      <CardContent>{children}</CardContent>
      <CardFooter>
        <Link
          to={to}
          className="text-xs font-medium text-primary hover:underline"
        >
          View all &rarr;
        </Link>
      </CardFooter>
    </Card>
  )
}

interface ResourceCardProps<T> {
  emptyText: string
  getKey: (item: T) => string | undefined
  icon: LucideIcon
  items: T[] | undefined
  primary: (item: T) => ReactNode
  secondary?: (item: T) => ReactNode
  title: string
  to: LinkProps["to"]
}

// A count, the first few items as id/detail rows, and a link to the list page.
function ResourceCard<T>({
  emptyText,
  getKey,
  icon: Icon,
  items,
  primary,
  secondary,
  title,
  to,
}: ResourceCardProps<T>) {
  const list = items ?? []

  return (
    <DashboardCard
      title={title}
      icon={<Icon className="size-3.5" />}
      count={list.length}
      to={to}
    >
      {list.length === 0 ? (
        <p className="text-xs text-muted-foreground">{emptyText}</p>
      ) : (
        <div className="space-y-0.5">
          {list.slice(0, MAX_ITEMS).map((item) => (
            <ItemRow key={getKey(item)}>
              <span className="font-mono">{primary(item)}</span>
              {secondary && (
                <span className="text-muted-foreground">{secondary(item)}</span>
              )}
            </ItemRow>
          ))}
          <Overflow total={list.length} />
        </div>
      )}
    </DashboardCard>
  )
}

function Overflow({ total }: { total: number }) {
  if (total <= MAX_ITEMS) {
    return null
  }

  return (
    <p className="mt-1 text-2xs text-muted-foreground">
      +{total - MAX_ITEMS} more
    </p>
  )
}

function ItemRow({ children }: { children: ReactNode }) {
  return (
    <div className="flex items-center justify-between py-0.5 text-xs">
      {children}
    </div>
  )
}

function InstancesCard() {
  const { data } = useQuery(ec2InstancesQueryOptions)
  const instances = data?.Reservations?.flatMap((r) => r.Instances ?? []) ?? []
  const active = instances.filter((i) => i.State?.Name !== "terminated")
  const running = instances.filter((i) => i.State?.Name === "running")
  const stopped = instances.filter((i) => i.State?.Name === "stopped")
  const pending = instances.filter((i) => i.State?.Name === "pending")
  const display = [...running, ...pending].slice(0, MAX_ITEMS)

  return (
    <DashboardCard
      title="Instances"
      icon={<HardDrive className="size-3.5" />}
      count={active.length}
      to="/ec2/describe-instances"
    >
      {active.length === 0 ? (
        <p className="text-xs text-muted-foreground">No instances</p>
      ) : (
        <>
          <div className="mb-2 flex gap-3 text-2xs text-muted-foreground">
            <span>{running.length} running</span>
            <span>{stopped.length} stopped</span>
          </div>
          <div className="space-y-0.5">
            {display.map((i) => (
              <ItemRow key={i.InstanceId}>
                <span className="font-mono">{i.InstanceId}</span>
                <span className="text-muted-foreground">{i.InstanceType}</span>
              </ItemRow>
            ))}
          </div>
          <Overflow total={running.length + pending.length} />
        </>
      )}
    </DashboardCard>
  )
}

function VolumesCard() {
  const { data } = useQuery(ec2VolumesQueryOptions)
  const volumes = data?.Volumes ?? []
  const totalGiB = volumes.reduce((sum, v) => sum + (v.Size ?? 0), 0)
  const inUse = volumes.filter((v) => v.State === "in-use")
  const available = volumes.filter((v) => v.State === "available")
  const display = volumes.slice(0, MAX_ITEMS)

  return (
    <DashboardCard
      title="Volumes"
      icon={<Box className="size-3.5" />}
      count={volumes.length}
      to="/ec2/describe-volumes"
    >
      {volumes.length === 0 ? (
        <p className="text-xs text-muted-foreground">No volumes</p>
      ) : (
        <>
          <div className="mb-2 flex gap-3 text-2xs text-muted-foreground">
            <span>{totalGiB} GiB total</span>
            <span>{inUse.length} in-use</span>
            <span>{available.length} available</span>
          </div>
          <div className="space-y-0.5">
            {display.map((v) => (
              <ItemRow key={v.VolumeId}>
                <span className="font-mono">{v.VolumeId}</span>
                <span className="flex items-center gap-2">
                  <span className="text-muted-foreground">{v.Size} GiB</span>
                  <StateBadge state={v.State} />
                </span>
              </ItemRow>
            ))}
          </div>
          <Overflow total={volumes.length} />
        </>
      )}
    </DashboardCard>
  )
}
