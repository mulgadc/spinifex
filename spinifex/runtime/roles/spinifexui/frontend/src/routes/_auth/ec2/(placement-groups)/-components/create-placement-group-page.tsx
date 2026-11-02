import { zodResolver } from "@hookform/resolvers/zod"
import {
  Controller,
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  commandFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
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
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (groupId?: string) => void
}

export function CreatePlacementGroupPage({
  onClose,
}: CreatePlacementGroupPageProps) {
  const finish = onClose

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
    let result

    try {
      result = await createMutation.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    finish(result.PlacementGroup?.GroupId)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
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
          error={createMutation.error}
          errorMsg="Failed to create placement group"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            finish()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Placement Group"
        />
      </FormLock>
    </form>
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
