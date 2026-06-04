import { useQuery } from "@tanstack/react-query"
import { Link, createFileRoute } from "@tanstack/react-router"
import {
  Box,
  Globe,
  HardDrive,
  Key,
  Network,
  FolderOpen,
  Shield,
  Waypoints,
} from "lucide-react"
import { Suspense, type ReactNode } from "react"

import { AdminDashboardPanel } from "@/components/admin-dashboard-panel"
import { PageHeading } from "@/components/page-heading"
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
      context.queryClient.ensureQueryData(ec2InstancesQueryOptions),
      context.queryClient.ensureQueryData(ec2VolumesQueryOptions),
      context.queryClient.ensureQueryData(ec2VpcsQueryOptions),
      context.queryClient.ensureQueryData(ec2KeyPairsQueryOptions),
      context.queryClient.ensureQueryData(s3BucketsQueryOptions),
      context.queryClient.ensureQueryData(ec2SecurityGroupsQueryOptions),
      context.queryClient.ensureQueryData(ec2NatGatewaysQueryOptions),
      context.queryClient.ensureQueryData(ec2AddressesQueryOptions),
    ])
  },
  head: () => ({
    meta: [{ title: "Dashboard | Mulga" }],
  }),
  component: Dashboard,
})

function Dashboard() {
  return (
    <>
      <PageHeading title="Dashboard" />
      <AdminDashboardPanel />
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
        <Suspense fallback={<CardSkeleton />}>
          <InstancesCard />
        </Suspense>
        <Suspense fallback={<CardSkeleton />}>
          <VolumesCard />
        </Suspense>
        <Suspense fallback={<CardSkeleton />}>
          <VpcsCard />
        </Suspense>
        <Suspense fallback={<CardSkeleton />}>
          <KeyPairsCard />
        </Suspense>
        <Suspense fallback={<CardSkeleton />}>
          <BucketsCard />
        </Suspense>
        <Suspense fallback={<CardSkeleton />}>
          <SecurityGroupsCard />
        </Suspense>
        <Suspense fallback={<CardSkeleton />}>
          <NatGatewaysCard />
        </Suspense>
        <Suspense fallback={<CardSkeleton />}>
          <ElasticIpsCard />
        </Suspense>
      </div>
    </>
  )
}

function CardSkeleton() {
  return (
    <Card>
      <CardHeader>
        <div className="h-4 w-32 animate-pulse rounded bg-muted" />
      </CardHeader>
      <CardContent>
        <div className="space-y-2">
          <div className="h-3 w-48 animate-pulse rounded bg-muted" />
          <div className="h-3 w-36 animate-pulse rounded bg-muted" />
        </div>
      </CardContent>
    </Card>
  )
}

interface DashboardCardProps {
  title: string
  icon: ReactNode
  count: number
  to: string
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
        <CardTitle className="flex items-center gap-2">
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

function Overflow({ total }: { total: number }) {
  if (total <= MAX_ITEMS) {
    return null
  }
  return (
    <p className="mt-1 text-[0.625rem] text-muted-foreground">
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
          <div className="mb-2 flex gap-3 text-[0.625rem] text-muted-foreground">
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
          <div className="mb-2 flex gap-3 text-[0.625rem] text-muted-foreground">
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

function VpcsCard() {
  const { data } = useQuery(ec2VpcsQueryOptions)
  const vpcs = data?.Vpcs ?? []
  const display = vpcs.slice(0, MAX_ITEMS)

  return (
    <DashboardCard
      title="VPCs"
      icon={<Network className="size-3.5" />}
      count={vpcs.length}
      to="/ec2/describe-vpcs"
    >
      {vpcs.length === 0 ? (
        <p className="text-xs text-muted-foreground">No VPCs</p>
      ) : (
        <div className="space-y-0.5">
          {display.map((vpc) => {
            const name = getNameTag(vpc.Tags)
            return (
              <ItemRow key={vpc.VpcId}>
                <span className="font-mono">
                  {vpc.VpcId}
                  {name && (
                    <span className="ml-1 font-sans text-muted-foreground">
                      ({name})
                    </span>
                  )}
                </span>
                <span className="text-muted-foreground">{vpc.CidrBlock}</span>
              </ItemRow>
            )
          })}
          <Overflow total={vpcs.length} />
        </div>
      )}
    </DashboardCard>
  )
}

function KeyPairsCard() {
  const { data } = useQuery(ec2KeyPairsQueryOptions)
  const keyPairs = data?.KeyPairs ?? []
  const display = keyPairs.slice(0, MAX_ITEMS)

  return (
    <DashboardCard
      title="Key Pairs"
      icon={<Key className="size-3.5" />}
      count={keyPairs.length}
      to="/ec2/describe-key-pairs"
    >
      {keyPairs.length === 0 ? (
        <p className="text-xs text-muted-foreground">No key pairs</p>
      ) : (
        <div className="space-y-0.5">
          {display.map((kp) => (
            <ItemRow key={kp.KeyPairId}>
              <span className="font-mono">{kp.KeyName}</span>
            </ItemRow>
          ))}
          <Overflow total={keyPairs.length} />
        </div>
      )}
    </DashboardCard>
  )
}

function BucketsCard() {
  const { data } = useQuery(s3BucketsQueryOptions)
  const buckets = data?.Buckets ?? []
  const display = buckets.slice(0, MAX_ITEMS)

  return (
    <DashboardCard
      title="S3 Buckets"
      icon={<FolderOpen className="size-3.5" />}
      count={buckets.length}
      to="/s3/ls"
    >
      {buckets.length === 0 ? (
        <p className="text-xs text-muted-foreground">No buckets</p>
      ) : (
        <div className="space-y-0.5">
          {display.map((b) => (
            <ItemRow key={b.Name}>
              <span className="font-mono">{b.Name}</span>
            </ItemRow>
          ))}
          <Overflow total={buckets.length} />
        </div>
      )}
    </DashboardCard>
  )
}

function SecurityGroupsCard() {
  const { data } = useQuery(ec2SecurityGroupsQueryOptions)
  const groups = data?.SecurityGroups ?? []
  const display = groups.slice(0, MAX_ITEMS)

  return (
    <DashboardCard
      title="Security Groups"
      icon={<Shield className="size-3.5" />}
      count={groups.length}
      to="/ec2/describe-security-groups"
    >
      {groups.length === 0 ? (
        <p className="text-xs text-muted-foreground">No security groups</p>
      ) : (
        <div className="space-y-0.5">
          {display.map((sg) => (
            <ItemRow key={sg.GroupId}>
              <span className="font-mono">{sg.GroupName}</span>
              <span className="text-muted-foreground">{sg.VpcId}</span>
            </ItemRow>
          ))}
          <Overflow total={groups.length} />
        </div>
      )}
    </DashboardCard>
  )
}

function NatGatewaysCard() {
  const { data } = useQuery(ec2NatGatewaysQueryOptions)
  const natGateways = data?.NatGateways ?? []
  const display = natGateways.slice(0, MAX_ITEMS)

  return (
    <DashboardCard
      title="NAT Gateways"
      icon={<Waypoints className="size-3.5" />}
      count={natGateways.length}
      to="/ec2/describe-nat-gateways"
    >
      {natGateways.length === 0 ? (
        <p className="text-xs text-muted-foreground">No NAT gateways</p>
      ) : (
        <div className="space-y-0.5">
          {display.map((nat) => (
            <ItemRow key={nat.NatGatewayId}>
              <span className="font-mono">{nat.NatGatewayId}</span>
              <span className="text-muted-foreground">{nat.State}</span>
            </ItemRow>
          ))}
          <Overflow total={natGateways.length} />
        </div>
      )}
    </DashboardCard>
  )
}

function ElasticIpsCard() {
  const { data } = useQuery(ec2AddressesQueryOptions)
  const addresses = data?.Addresses ?? []
  const display = addresses.slice(0, MAX_ITEMS)

  return (
    <DashboardCard
      title="Elastic IPs"
      icon={<Globe className="size-3.5" />}
      count={addresses.length}
      to="/ec2/describe-addresses"
    >
      {addresses.length === 0 ? (
        <p className="text-xs text-muted-foreground">No Elastic IPs</p>
      ) : (
        <div className="space-y-0.5">
          {display.map((addr) => (
            <ItemRow key={addr.AllocationId}>
              <span className="font-mono">{addr.PublicIp}</span>
              <span className="text-muted-foreground">
                {addr.AssociationId ? "associated" : "available"}
              </span>
            </ItemRow>
          ))}
          <Overflow total={addresses.length} />
        </div>
      )}
    </DashboardCard>
  )
}
