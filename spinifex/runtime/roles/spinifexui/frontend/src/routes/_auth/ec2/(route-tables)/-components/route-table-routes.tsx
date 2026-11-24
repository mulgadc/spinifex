import type { RouteTable } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { Plus, Trash2 } from "lucide-react"
import { useState } from "react"
import { Controller, useForm, useWatch } from "react-hook-form"

import { type DataColumn, DataTable } from "@/components/data-table"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import {
  type PickerNotice,
  pickerNotice,
  PickerNoticeText,
} from "@/components/picker-notice"
import { StateBadge } from "@/components/state-badge"
import { TruncatedText } from "@/components/table-cells"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { routableTransitGateways } from "@/lib/transit-gateway"
import { getNameTag } from "@/lib/utils"
import { isLivePeering, peerOf, touchesVpc } from "@/lib/vpc-peering"
import { useCreateRoute, useDeleteRoute } from "@/mutations/ec2"
import {
  ec2InternetGatewaysQueryOptions,
  ec2NatGatewaysQueryOptions,
  ec2TransitGatewaysByIdQueryOptions,
  ec2VpcLiveTransitGatewayAttachmentsQueryOptions,
  ec2VpcPeeringConnectionsQueryOptions,
} from "@/queries/ec2"
import { type CreateRouteFormData, createRouteSchema } from "@/types/ec2"

type RouteEntry = NonNullable<RouteTable["Routes"]>[number]

function routeDestination(route: RouteEntry): string {
  return route.DestinationCidrBlock ?? route.DestinationPrefixListId ?? "—"
}

export function routeTarget(route: RouteEntry): string {
  return (
    route.GatewayId ??
    route.NatGatewayId ??
    route.NetworkInterfaceId ??
    route.InstanceId ??
    route.VpcPeeringConnectionId ??
    route.TransitGatewayId ??
    "—"
  )
}

// The table's routes, with add and delete. Only targets the table's own VPC can
// reach are offered; the local route cannot be deleted.
function routeColumns(
  deleting: boolean,
  onDelete: (cidr: string) => void,
): DataColumn<RouteEntry>[] {
  return [
    {
      id: "destination",
      header: "Destination",
      cell: (route) => (
        <span className="font-mono text-xs">{routeDestination(route)}</span>
      ),
    },
    {
      id: "target",
      header: "Target",
      cell: (route) => (
        <TruncatedText
          className="max-w-32 font-mono text-xs"
          text={routeTarget(route)}
        />
      ),
    },
    {
      id: "state",
      header: "Status",
      cell: (route) => <StateBadge state={route.State} />,
    },
    {
      // The delete button carries its own label, and a header word would push
      // the column past a panel's width.
      id: "actions",
      header: "",
      align: "end",
      cell: (route) => {
        const cidr = route.DestinationCidrBlock

        return (
          route.GatewayId !== "local" &&
          cidr && (
            <Button
              aria-label={`Delete route to ${cidr}`}
              disabled={deleting}
              onClick={() => {
                onDelete(cidr)
              }}
              size="icon-sm"
              variant="ghost"
            >
              <Trash2 />
            </Button>
          )
        )
      },
    },
  ]
}

interface RouteTargets {
  targets: { id: string; label: string }[]
  notice: PickerNotice | undefined
}

// The transit gateways the VPC can route to, read only once the user picks that
// target type, and scoped to the VPC's own attachments.
function useTransitGatewayTargets(
  vpcId: string,
  enabled: boolean,
): RouteTargets {
  const attachments = useQuery({
    ...ec2VpcLiveTransitGatewayAttachmentsQueryOptions(vpcId),
    enabled,
  })

  const attachmentList = attachments.data?.TransitGatewayVpcAttachments ?? []

  const ids = [
    ...new Set(attachmentList.flatMap((att) => att.TransitGatewayId ?? [])),
  ].toSorted()

  const gateways = useQuery({
    ...ec2TransitGatewaysByIdQueryOptions(ids),
    enabled: enabled && ids.length > 0,
  })

  const routable = routableTransitGateways(
    gateways.data?.TransitGateways ?? [],
    attachmentList,
    vpcId,
  )

  const messages = {
    loading: "Loading transit gateways…",
    failed: "Could not load transit gateways",
    empty: "No available transit gateway is attached to this VPC.",
  }

  const notice =
    pickerNotice(attachments, ids.length === 0, messages) ??
    pickerNotice(gateways, routable.length === 0, messages)

  return {
    notice,
    targets: routable.map((tgw) => {
      const id = tgw.TransitGatewayId ?? ""
      const name = getNameTag(tgw.Tags)

      return { id, label: name ? `${id} (${name})` : id }
    }),
  }
}

export function RouteTableRoutes({ rtb }: { rtb: RouteTable }) {
  const routeTableId = rtb.RouteTableId ?? ""
  const routes = rtb.Routes ?? []
  const { data: igwData } = useQuery(ec2InternetGatewaysQueryOptions)
  const { data: natData } = useQuery(ec2NatGatewaysQueryOptions)
  const { data: pcxData } = useQuery(ec2VpcPeeringConnectionsQueryOptions)
  const createRoute = useCreateRoute()
  const deleteRoute = useDeleteRoute()
  const [routeToDelete, setRouteToDelete] = useState<string | null>(null)

  const form = useForm<CreateRouteFormData>({
    resolver: zodResolver(createRouteSchema),
    defaultValues: {
      destinationCidrBlock: "0.0.0.0/0",
      targetType: "igw",
      targetId: "",
    },
  })

  const targetType = useWatch({ control: form.control, name: "targetType" })

  const attachedIgws = (igwData?.InternetGateways ?? []).filter((igw) =>
    igw.Attachments?.some((att) => att.VpcId === rtb.VpcId),
  )

  const vpcNatGateways = (natData?.NatGateways ?? []).filter(
    (nat) => nat.VpcId === rtb.VpcId && nat.State === "available",
  )

  // A pending connection is a valid target; its route reads blackhole until accepted.
  const vpcPeerings = (pcxData?.VpcPeeringConnections ?? []).filter(
    (pcx) => touchesVpc(pcx, rtb.VpcId) && isLivePeering(pcx),
  )

  const transitGateways = useTransitGatewayTargets(
    rtb.VpcId ?? "",
    targetType === "tgw",
  )

  const targetNotice = targetType === "tgw" ? transitGateways.notice : undefined

  const targets = {
    igw: attachedIgws.map((igw) => ({
      id: igw.InternetGatewayId ?? "",
      label: igw.InternetGatewayId ?? "",
    })),
    nat: vpcNatGateways.map((nat) => ({
      id: nat.NatGatewayId ?? "",
      label: nat.NatGatewayId ?? "",
    })),
    pcx: vpcPeerings.map((pcx) => {
      const id = pcx.VpcPeeringConnectionId ?? ""
      const name = getNameTag(pcx.Tags)
      const peer = peerOf(pcx, rtb.VpcId)

      return {
        id,
        label: `${id} (${name ?? `${peer?.VpcId ?? ""} ${peer?.CidrBlock ?? ""}`.trim()})`,
      }
    }),
    tgw: transitGateways.targets,
  }[targetType]

  const error = createRoute.error ?? deleteRoute.error

  async function onDeleteRoute() {
    if (!routeToDelete) {
      return
    }

    try {
      await deleteRoute.mutateAsync({
        routeTableId,
        destinationCidrBlock: routeToDelete,
      })
    } catch {
      // Shown by the banner above the form.
    } finally {
      setRouteToDelete(null)
    }
  }

  async function onAddRoute(values: CreateRouteFormData) {
    try {
      await createRoute.mutateAsync({
        routeTableId,
        destinationCidrBlock: values.destinationCidrBlock,
        gatewayId: values.targetType === "igw" ? values.targetId : undefined,
        natGatewayId: values.targetType === "nat" ? values.targetId : undefined,
        vpcPeeringConnectionId:
          values.targetType === "pcx" ? values.targetId : undefined,
        transitGatewayId:
          values.targetType === "tgw" ? values.targetId : undefined,
      })
    } catch {
      // Shown by the banner above the form.
      return
    }

    form.reset()
  }

  return (
    <PanelSection title={`Routes (${routes.length})`}>
      <div className="flex flex-col gap-4">
        <DataTable
          caption="Routes"
          columns={routeColumns(deleteRoute.isPending, setRouteToDelete)}
          empty="No routes."
          getRowId={routeDestination}
          rows={routes}
        />

        {error && <ErrorBanner error={error} msg="Failed to update route" />}

        <form
          className="flex flex-col gap-3"
          onSubmit={form.handleSubmit(onAddRoute)}
        >
          <Field>
            <FieldTitle>
              <label htmlFor="destinationCidrBlock">Destination</label>
            </FieldTitle>
            <Input
              id="destinationCidrBlock"
              placeholder="0.0.0.0/0"
              {...form.register("destinationCidrBlock")}
            />
            <FieldError errors={[form.formState.errors.destinationCidrBlock]} />
          </Field>

          <div className="grid gap-3 sm:grid-cols-2">
            <Field>
              <FieldTitle>
                <label htmlFor="targetType">Target type</label>
              </FieldTitle>
              <Controller
                control={form.control}
                name="targetType"
                render={({ field }) => (
                  <Select
                    onValueChange={(value) => {
                      field.onChange(value)
                      form.setValue("targetId", "")
                    }}
                    value={field.value}
                  >
                    <SelectTrigger className="w-full" id="targetType">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="igw">Internet gateway</SelectItem>
                      <SelectItem value="nat">NAT gateway</SelectItem>
                      <SelectItem value="pcx">Peering connection</SelectItem>
                      <SelectItem value="tgw">Transit gateway</SelectItem>
                    </SelectContent>
                  </Select>
                )}
              />
            </Field>

            <Field>
              <FieldTitle>
                <label htmlFor="targetId">Target</label>
              </FieldTitle>
              <Controller
                control={form.control}
                name="targetId"
                render={({ field }) =>
                  targetNotice ? (
                    <PickerNoticeText notice={targetNotice} />
                  ) : (
                    <Select onValueChange={field.onChange} value={field.value}>
                      <SelectTrigger
                        aria-invalid={!!form.formState.errors.targetId}
                        className="w-full"
                        id="targetId"
                      >
                        <SelectValue placeholder="Select target" />
                      </SelectTrigger>
                      <SelectContent>
                        {targets.map((target) => (
                          <SelectItem key={target.id} value={target.id}>
                            {target.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  )
                }
              />
              <FieldError errors={[form.formState.errors.targetId]} />
            </Field>
          </div>

          <div>
            <Button disabled={createRoute.isPending} size="sm" type="submit">
              <Plus data-icon="inline-start" />
              Add route
            </Button>
          </div>
        </form>
      </div>

      <DeleteConfirmationDialog
        description={`Delete the route to ${routeToDelete} from "${routeTableId}"? Traffic to that destination from every subnet using this table stops being routed.`}
        isPending={deleteRoute.isPending}
        onConfirm={() => {
          void onDeleteRoute()
        }}
        onOpenChange={(open) => {
          if (!open) {
            setRouteToDelete(null)
          }
        }}
        open={routeToDelete !== null}
        title="Delete route"
      />
    </PanelSection>
  )
}
