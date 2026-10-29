import { zodResolver } from "@hookform/resolvers/zod"
import { useNavigate } from "@tanstack/react-router"
import {
  Controller,
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
  commandFlag,
  shellSingleQuote,
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

interface CreatePlacementGroupPageProps {
  // Given when the form sits in the list's side panel: called with the new
  // group's ID once created, or with nothing on cancel.
  onClose?: (groupId?: string) => void
}

export function CreatePlacementGroupPage({
  onClose,
}: CreatePlacementGroupPageProps) {
  const navigate = useNavigate()

  async function finish(groupId?: string) {
    if (onClose) {
      onClose(groupId)

      return
    }

    await (groupId
      ? navigate({
          to: "/ec2/describe-placement-groups/$id",
          params: { id: groupId },
        })
      : navigate({ to: "/ec2/describe-placement-groups" }))
  }

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

  const onSubmit = async (data: CreatePlacementGroupFormData) => {
    const result = await createMutation.mutateAsync(data)
    await finish(result.PlacementGroup?.GroupId)
  }

  return (
    <>
      {!onClose && (
        <>
          <BackLink to="/ec2/describe-placement-groups">
            Back to placement groups
          </BackLink>
          <PageHeading title="Create Placement Group" />
        </>
      )}

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
              <Select onValueChange={field.onChange} value={field.value ?? ""}>
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

        <CliCommandPanel commands={buildCreatePlacementGroupCommands(values)} />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => {
            await finish()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Placement Group"
        />
      </form>
    </>
  )
}

function buildCreatePlacementGroupCommands(
  values: DeepPartialSkipArrayKey<CreatePlacementGroupFormData>,
): CliCommand[] {
  const name = values.groupName ?? ""

  return [
    {
      label: "Create Placement Group",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 create-placement-group",
        },
        ...commandFlag(
          "\\\n  --group-name",
          name ? shellSingleQuote(name) : "<GroupName>",
        ),
        ...commandFlag("\\\n  --strategy", values.strategy || "spread"),
      ],
    },
  ]
}
