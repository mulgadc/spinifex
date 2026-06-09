import { zodResolver } from "@hookform/resolvers/zod"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { Controller, useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
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
import { useCreatePlacementGroup } from "@/mutations/ec2"
import {
  type CreatePlacementGroupFormData,
  createPlacementGroupSchema,
} from "@/types/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(placement-groups)/create-placement-group",
)({
  head: () => ({
    meta: [
      {
        title: "Create Placement Group | EC2 | Mulga",
      },
    ],
  }),
  component: CreatePlacementGroup,
})

function CreatePlacementGroup() {
  const navigate = useNavigate()
  const createMutation = useCreatePlacementGroup()

  const {
    control,
    handleSubmit,
    register,
    formState: { errors, isSubmitting },
  } = useForm<CreatePlacementGroupFormData>({
    resolver: zodResolver(createPlacementGroupSchema),
    defaultValues: {
      groupName: "",
      strategy: "spread",
    },
  })

  const values = useWatch({ control })
  const cliWatch = (name?: string): unknown =>
    name ? (values as Record<string, unknown>)[name] : undefined

  const onSubmit = async (data: CreatePlacementGroupFormData) => {
    const result = await createMutation.mutateAsync(data)
    navigate({
      to: "/ec2/describe-placement-groups/$id",
      params: { id: result.PlacementGroup?.GroupId ?? "" },
    })
  }

  return (
    <>
      <BackLink to="/ec2/describe-placement-groups">
        Back to placement groups
      </BackLink>

      <PageHeading title="Create Placement Group" />

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create placement group"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <Field>
          <FieldTitle>
            <label htmlFor="groupName">Group Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.groupName}
            id="groupName"
            placeholder="my-placement-group"
            {...register("groupName")}
          />
          <FieldError errors={[errors.groupName]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="strategy">Strategy</label>
          </FieldTitle>
          <Controller
            control={control}
            name="strategy"
            render={({ field }) => (
              <Select
                onValueChange={(value) => field.onChange(value)}
                value={field.value ?? ""}
              >
                <SelectTrigger
                  aria-invalid={!!errors.strategy}
                  className="w-full"
                  id="strategy"
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="spread">spread</SelectItem>
                  <SelectItem value="cluster">cluster</SelectItem>
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.strategy]} />
        </Field>

        <CliCommandPanel
          commands={buildCreatePlacementGroupCommands(cliWatch)}
        />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/ec2/describe-placement-groups" })
          }
          pendingLabel="Creating…"
          submitLabel="Create Placement Group"
        />
      </form>
    </>
  )
}

function shellSingleQuote(value: string): string {
  return `'${value.replaceAll("'", `'"'"'`)}'`
}

function buildCreatePlacementGroupCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawName = watch("groupName")
  const name = typeof rawName === "string" ? rawName : ""
  const rawStrategy = watch("strategy")
  const strategy = typeof rawStrategy === "string" ? rawStrategy : ""
  const nameValue = name ? shellSingleQuote(name) : "<GroupName>"

  return [
    {
      label: "Create Placement Group",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 create-placement-group",
        },
        { type: "flag", value: " \\\n  --group-name" },
        { type: "value", value: ` ${nameValue}` },
        { type: "flag", value: " \\\n  --strategy" },
        { type: "value", value: ` ${strategy || "spread"}` },
      ],
    },
  ]
}
