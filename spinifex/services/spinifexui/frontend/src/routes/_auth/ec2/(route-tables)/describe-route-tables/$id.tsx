import type { RouteTable } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"
import { Controller, useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { Badge } from "@/components/ui/badge"
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
import { getNameTag } from "@/lib/utils"
import {
  useAssociateRouteTable,
  useCreateRoute,
  useDeleteRoute,
  useDeleteRouteTable,
  useDisassociateRouteTable,
} from "@/mutations/ec2"
import {
  ec2InternetGatewaysQueryOptions,
  ec2NatGatewaysQueryOptions,
  ec2RouteTableQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import {
  type AssociateRouteTableFormData,
  type CreateRouteFormData,
  associateRouteTableSchema,
  createRouteSchema,
} from "@/types/ec2"

type RouteEntry = NonNullable<RouteTable["Routes"]>[number]

export const Route = createFileRoute(
  "/_auth/ec2/(route-tables)/describe-route-tables/$id",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2RouteTableQueryOptions(params.id)),
      context.queryClient.ensureQueryData(ec2InternetGatewaysQueryOptions),
      context.queryClient.ensureQueryData(ec2NatGatewaysQueryOptions),
      context.queryClient.ensureQueryData(ec2SubnetsQueryOptions),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Route Table | Mulga`,
      },
    ],
  }),
  component: RouteTableDetail,
})

function routeTarget(route: RouteEntry): string {
  return (
    route.GatewayId ??
    route.NatGatewayId ??
    route.NetworkInterfaceId ??
    route.InstanceId ??
    route.VpcPeeringConnectionId ??
    "—"
  )
}

function RouteTableDetail() {
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ec2RouteTableQueryOptions(id))
  const { data: igwData } = useSuspenseQuery(ec2InternetGatewaysQueryOptions)
  const { data: natData } = useSuspenseQuery(ec2NatGatewaysQueryOptions)
  const { data: subnetData } = useSuspenseQuery(ec2SubnetsQueryOptions)

  const deleteRouteTable = useDeleteRouteTable()
  const createRoute = useCreateRoute()
  const deleteRoute = useDeleteRoute()
  const associate = useAssociateRouteTable()
  const disassociate = useDisassociateRouteTable()

  const [showDeleteDialog, setShowDeleteDialog] = useState(false)

  const rtb = data.RouteTables?.[0]

  const routeForm = useForm<CreateRouteFormData>({
    resolver: zodResolver(createRouteSchema),
    defaultValues: {
      destinationCidrBlock: "0.0.0.0/0",
      targetType: "igw",
      targetId: "",
    },
  })
  const targetType = useWatch({
    control: routeForm.control,
    name: "targetType",
  })

  const associateForm = useForm<AssociateRouteTableFormData>({
    resolver: zodResolver(associateRouteTableSchema),
    defaultValues: { subnetId: "" },
  })

  if (!rtb?.RouteTableId) {
    return (
      <>
        <BackLink to="/ec2/describe-route-tables">
          Back to Route Tables
        </BackLink>
        <p className="text-muted-foreground">Route Table not found.</p>
      </>
    )
  }

  const routeTableId = rtb.RouteTableId
  const name = getNameTag(rtb.Tags)
  const isMain = rtb.Associations?.some((a) => a.Main) ?? false
  const routes = rtb.Routes ?? []
  const subnetAssociations = (rtb.Associations ?? []).filter((a) => a.SubnetId)

  const attachedIgws = (igwData.InternetGateways ?? []).filter((igw) =>
    igw.Attachments?.some((att) => att.VpcId === rtb.VpcId),
  )
  const vpcNatGateways = (natData.NatGateways ?? []).filter(
    (nat) => nat.VpcId === rtb.VpcId && nat.State === "available",
  )
  const associatedSubnetIds = new Set(subnetAssociations.map((a) => a.SubnetId))
  const availableSubnets = (subnetData.Subnets ?? []).filter(
    (s) => s.VpcId === rtb.VpcId && !associatedSubnetIds.has(s.SubnetId),
  )

  const handleDeleteRouteTable = async () => {
    try {
      await deleteRouteTable.mutateAsync(routeTableId)
      navigate({ to: "/ec2/describe-route-tables" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  const onAddRoute = async (values: CreateRouteFormData) => {
    await createRoute.mutateAsync({
      routeTableId,
      destinationCidrBlock: values.destinationCidrBlock,
      gatewayId: values.targetType === "igw" ? values.targetId : undefined,
      natGatewayId: values.targetType === "nat" ? values.targetId : undefined,
    })
    routeForm.reset({
      destinationCidrBlock: "0.0.0.0/0",
      targetType: "igw",
      targetId: "",
    })
  }

  const onAssociate = async (values: AssociateRouteTableFormData) => {
    await associate.mutateAsync({
      routeTableId,
      subnetId: values.subnetId,
    })
    associateForm.reset({ subnetId: "" })
  }

  return (
    <>
      <BackLink to="/ec2/describe-route-tables">Back to Route Tables</BackLink>

      {deleteRouteTable.error && (
        <ErrorBanner
          error={deleteRouteTable.error}
          msg="Failed to delete Route Table"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <Button
                disabled={isMain || deleteRouteTable.isPending}
                onClick={() => setShowDeleteDialog(true)}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete
              </Button>
              {isMain && <Badge variant="secondary">Main</Badge>}
            </div>
          }
          subtitle="Route Table Details"
          title={name ? `${rtb.RouteTableId} (${name})` : rtb.RouteTableId}
        />

        <DetailCard>
          <DetailCard.Header>Route Table Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Route Table ID" value={rtb.RouteTableId} />
            <DetailRow label="Main" value={isMain ? "Yes" : "No"} />
            <DetailRow
              label="VPC ID"
              value={
                rtb.VpcId ? (
                  <Link
                    className="text-primary hover:underline"
                    params={{ id: rtb.VpcId }}
                    to="/ec2/describe-vpcs/$id"
                  >
                    {rtb.VpcId}
                  </Link>
                ) : (
                  "—"
                )
              }
            />
            <DetailRow label="Owner ID" value={rtb.OwnerId ?? "—"} />
          </DetailCard.Content>
        </DetailCard>

        <DetailCard>
          <DetailCard.Header>Routes</DetailCard.Header>
          <div className="space-y-4 p-4">
            <div className="overflow-x-auto rounded-md border">
              <table className="w-full text-sm">
                <thead className="bg-muted/50 text-left text-muted-foreground">
                  <tr>
                    <th className="p-2 font-medium">Destination</th>
                    <th className="p-2 font-medium">Target</th>
                    <th className="p-2 font-medium">Status</th>
                    <th className="p-2">
                      <span className="sr-only">Actions</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {routes.length > 0 ? (
                    routes.map((route) => {
                      const destination =
                        route.DestinationCidrBlock ??
                        route.DestinationPrefixListId ??
                        "—"
                      const isLocal = route.GatewayId === "local"
                      const cidr = route.DestinationCidrBlock
                      return (
                        <tr className="border-t" key={destination}>
                          <td className="p-2 font-mono">{destination}</td>
                          <td className="p-2 font-mono">
                            {routeTarget(route)}
                          </td>
                          <td className="p-2">{route.State ?? "—"}</td>
                          <td className="p-2 text-right">
                            {!isLocal && cidr && (
                              <Button
                                disabled={deleteRoute.isPending}
                                onClick={async () =>
                                  await deleteRoute.mutateAsync({
                                    routeTableId,
                                    destinationCidrBlock: cidr,
                                  })
                                }
                                size="sm"
                                variant="ghost"
                              >
                                <Trash2 className="size-4" />
                              </Button>
                            )}
                          </td>
                        </tr>
                      )
                    })
                  ) : (
                    <tr>
                      <td className="p-2 text-muted-foreground" colSpan={4}>
                        No routes.
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>

            {(createRoute.error ?? deleteRoute.error) && (
              <ErrorBanner
                error={createRoute.error ?? deleteRoute.error ?? undefined}
                msg="Failed to update route"
              />
            )}

            <form
              className="grid items-end gap-3 sm:grid-cols-[1fr_1fr_1fr_auto]"
              onSubmit={routeForm.handleSubmit(onAddRoute)}
            >
              <Field>
                <FieldTitle>
                  <label htmlFor="destinationCidrBlock">Destination</label>
                </FieldTitle>
                <Input
                  id="destinationCidrBlock"
                  placeholder="0.0.0.0/0"
                  {...routeForm.register("destinationCidrBlock")}
                />
                <FieldError
                  errors={[routeForm.formState.errors.destinationCidrBlock]}
                />
              </Field>

              <Field>
                <FieldTitle>
                  <label htmlFor="targetType">Target type</label>
                </FieldTitle>
                <Controller
                  control={routeForm.control}
                  name="targetType"
                  render={({ field }) => (
                    <Select
                      onValueChange={(value) => {
                        field.onChange(value)
                        routeForm.setValue("targetId", "")
                      }}
                      value={field.value}
                    >
                      <SelectTrigger className="w-full" id="targetType">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="igw">Internet Gateway</SelectItem>
                        <SelectItem value="nat">NAT Gateway</SelectItem>
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
                  control={routeForm.control}
                  name="targetId"
                  render={({ field }) => (
                    <Select
                      onValueChange={(value) => field.onChange(value)}
                      value={field.value ?? ""}
                    >
                      <SelectTrigger
                        aria-invalid={!!routeForm.formState.errors.targetId}
                        className="w-full"
                        id="targetId"
                      >
                        <SelectValue placeholder="Select target" />
                      </SelectTrigger>
                      <SelectContent>
                        {targetType === "igw"
                          ? attachedIgws.map((igw) => (
                              <SelectItem
                                key={igw.InternetGatewayId}
                                value={igw.InternetGatewayId ?? ""}
                              >
                                {igw.InternetGatewayId}
                              </SelectItem>
                            ))
                          : vpcNatGateways.map((nat) => (
                              <SelectItem
                                key={nat.NatGatewayId}
                                value={nat.NatGatewayId ?? ""}
                              >
                                {nat.NatGatewayId}
                              </SelectItem>
                            ))}
                      </SelectContent>
                    </Select>
                  )}
                />
                <FieldError errors={[routeForm.formState.errors.targetId]} />
              </Field>

              <Button disabled={createRoute.isPending} size="sm" type="submit">
                Add route
              </Button>
            </form>
          </div>
        </DetailCard>

        <DetailCard>
          <DetailCard.Header>Subnet Associations</DetailCard.Header>
          <div className="space-y-4 p-4">
            {subnetAssociations.length > 0 ? (
              <ul className="space-y-2">
                {subnetAssociations.map((assoc) => {
                  const associationId = assoc.RouteTableAssociationId
                  return (
                    <li
                      className="flex items-center justify-between rounded-md border p-2 text-sm"
                      key={associationId}
                    >
                      <Link
                        className="font-mono text-primary hover:underline"
                        params={{ id: assoc.SubnetId ?? "" }}
                        to="/ec2/describe-subnets/$id"
                      >
                        {assoc.SubnetId}
                      </Link>
                      <Button
                        disabled={!associationId || disassociate.isPending}
                        onClick={async () => {
                          if (associationId) {
                            await disassociate.mutateAsync(associationId)
                          }
                        }}
                        size="sm"
                        variant="ghost"
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </li>
                  )
                })}
              </ul>
            ) : (
              <p className="text-sm text-muted-foreground">
                No subnet associations.
              </p>
            )}

            {(associate.error ?? disassociate.error) && (
              <ErrorBanner
                error={associate.error ?? disassociate.error ?? undefined}
                msg="Failed to update association"
              />
            )}

            <form
              className="grid items-end gap-3 sm:grid-cols-[1fr_auto]"
              onSubmit={associateForm.handleSubmit(onAssociate)}
            >
              <Field>
                <FieldTitle>
                  <label htmlFor="subnetId">Associate subnet</label>
                </FieldTitle>
                <Controller
                  control={associateForm.control}
                  name="subnetId"
                  render={({ field }) => (
                    <Select
                      onValueChange={(value) => field.onChange(value)}
                      value={field.value ?? ""}
                    >
                      <SelectTrigger
                        aria-invalid={!!associateForm.formState.errors.subnetId}
                        className="w-full"
                        id="subnetId"
                      >
                        <SelectValue placeholder="Select a subnet" />
                      </SelectTrigger>
                      <SelectContent>
                        {availableSubnets.map((subnet) => {
                          const subnetName = getNameTag(subnet.Tags)
                          return (
                            <SelectItem
                              key={subnet.SubnetId}
                              value={subnet.SubnetId ?? ""}
                            >
                              {subnetName
                                ? `${subnet.SubnetId} (${subnetName})`
                                : `${subnet.SubnetId} (${subnet.CidrBlock})`}
                            </SelectItem>
                          )
                        })}
                      </SelectContent>
                    </Select>
                  )}
                />
                <FieldError
                  errors={[associateForm.formState.errors.subnetId]}
                />
              </Field>

              <Button disabled={associate.isPending} size="sm" type="submit">
                Associate
              </Button>
            </form>
          </div>
        </DetailCard>
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the Route Table "${rtb.RouteTableId}"? This action cannot be undone.`}
        isPending={deleteRouteTable.isPending}
        onConfirm={handleDeleteRouteTable}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete Route Table"
      />
    </>
  )
}
