import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { TextField } from "@/components/form-fields"
import { PageHeading } from "@/components/page-heading"
import { VpcSelectField } from "@/components/vpc-select-field"
import { useCreateRouteTable } from "@/mutations/ec2"
import { ec2VpcsQueryOptions } from "@/queries/ec2"
import {
  type CreateRouteTableFormData,
  createRouteTableSchema,
} from "@/types/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(route-tables)/create-route-table",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2VpcsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Create Route Table | EC2 | Mulga",
      },
    ],
  }),
  component: CreateRouteTable,
})

function CreateRouteTable() {
  const navigate = useNavigate()
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const createMutation = useCreateRouteTable()

  const vpcs = vpcsData.Vpcs ?? []

  const {
    control,
    handleSubmit,
    register,
    formState: { isSubmitting },
  } = useForm<CreateRouteTableFormData>({
    resolver: zodResolver(createRouteTableSchema),
    defaultValues: {
      vpcId: "",
      name: "",
    },
  })

  const onSubmit = async (data: CreateRouteTableFormData) => {
    const result = await createMutation.mutateAsync({
      vpcId: data.vpcId,
      name: data.name?.trim() ? data.name.trim() : undefined,
    })

    const routeTableId = result.RouteTable?.RouteTableId

    if (routeTableId) {
      navigate({
        to: "/ec2/describe-route-tables/$id",
        params: { id: routeTableId },
      })
    } else {
      navigate({ to: "/ec2/describe-route-tables" })
    }
  }

  return (
    <>
      <BackLink to="/ec2/describe-route-tables">Back to Route Tables</BackLink>

      <PageHeading title="Create Route Table" />

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create Route Table"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <TextField
          id="name"
          label="Name (optional)"
          placeholder="my-route-table"
          {...register("name")}
        />

        <VpcSelectField
          control={control}
          id="vpcId"
          name="vpcId"
          placeholder="Select a VPC"
          vpcs={vpcs}
        />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => {
            await navigate({ to: "/ec2/describe-route-tables" })
          }}
          pendingLabel="Creating…"
          submitLabel="Create Route Table"
        />
      </form>
    </>
  )
}
