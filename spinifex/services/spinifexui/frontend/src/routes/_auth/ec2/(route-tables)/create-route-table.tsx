import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { Controller, useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
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
    await context.queryClient.ensureQueryData(ec2VpcsQueryOptions)
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
    formState: { errors, isSubmitting },
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
        <Field>
          <FieldTitle>
            <label htmlFor="name">Name (optional)</label>
          </FieldTitle>
          <Input id="name" placeholder="my-route-table" {...register("name")} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="vpcId">VPC</label>
          </FieldTitle>
          <Controller
            control={control}
            name="vpcId"
            render={({ field }) => (
              <Select
                onValueChange={(value) => field.onChange(value)}
                value={field.value ?? ""}
              >
                <SelectTrigger
                  aria-invalid={!!errors.vpcId}
                  className="w-full"
                  id="vpcId"
                >
                  <SelectValue placeholder="Select a VPC" />
                </SelectTrigger>
                <SelectContent>
                  {vpcs.map((vpc) => {
                    const name = getNameTag(vpc.Tags)
                    return (
                      <SelectItem key={vpc.VpcId} value={vpc.VpcId ?? ""}>
                        {name
                          ? `${vpc.VpcId} (${name})`
                          : `${vpc.VpcId} (${vpc.CidrBlock})`}
                      </SelectItem>
                    )
                  })}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.vpcId]} />
        </Field>

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/ec2/describe-route-tables" })
          }
          pendingLabel="Creating…"
          submitLabel="Create Route Table"
        />
      </form>
    </>
  )
}
