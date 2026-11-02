import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Controller, useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  cliPlaceholder,
  commandFlag,
  type CliCommand,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { TagsFieldArray } from "@/components/tags-field-array"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useCreateDBParameterGroup } from "@/mutations/rds"
import { rdsEngineVersionsQueryOptions } from "@/queries/rds"
import {
  type CreateDBParameterGroupFormData,
  createDBParameterGroupSchema,
} from "@/types/rds"

interface CreateDBParameterGroupPageProps {
  // Called with the new parameter group's identifier once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateDBParameterGroupPage({
  onClose,
}: CreateDBParameterGroupPageProps) {
  const createGroup = useCreateDBParameterGroup()

  const { data: engineVersionsData } = useSuspenseQuery(
    rdsEngineVersionsQueryOptions,
  )

  // The families come from the engine catalog rather than a table here, so a
  // version pin bump moves the picker without the console being edited.
  const families = [
    ...new Map(
      (engineVersionsData.DBEngineVersions ?? [])
        .filter((v) => v.DBParameterGroupFamily)
        .map((v) => [
          v.DBParameterGroupFamily ?? "",
          v.DBEngineVersionDescription ?? v.Engine ?? "",
        ]),
    ),
  ]

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
  } = useForm<CreateDBParameterGroupFormData>({
    resolver: zodResolver(createDBParameterGroupSchema),
    defaultValues: {
      dbParameterGroupName: "",
      dbParameterGroupFamily: families[0]?.[0] ?? "",
      description: "",
      tags: [],
    },
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateDBParameterGroupFormData) => {
    try {
      await createGroup.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(data.dbParameterGroupName)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createGroup.isPending || isSubmitting}
      >
        <Field>
          <FieldTitle>
            <label htmlFor="parameter-group-name">Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.dbParameterGroupName}
            id="parameter-group-name"
            placeholder="orders-postgres18"
            {...register("dbParameterGroupName")}
          />
          <FieldDescription>
            Letters, digits and hyphens, starting with a letter. The service
            reserves the name &quot;default&quot; and the &quot;default.&quot;
            prefix.
          </FieldDescription>
          <FieldError errors={[errors.dbParameterGroupName]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="parameter-group-family">Family</label>
          </FieldTitle>
          <Controller
            control={control}
            name="dbParameterGroupFamily"
            render={({ field }) => (
              <Select onValueChange={field.onChange} value={field.value}>
                <SelectTrigger
                  aria-invalid={!!errors.dbParameterGroupFamily}
                  className="w-full"
                  id="parameter-group-family"
                >
                  <SelectValue placeholder="Select a family" />
                </SelectTrigger>
                <SelectContent>
                  {families.map(([family, description]) => (
                    <SelectItem key={family} value={family}>
                      {family} — {description}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <FieldDescription>
            The family fixes which engine can attach the group, and it cannot be
            changed afterwards. A group of one family is refused by an instance
            of the other engine.
          </FieldDescription>
          <FieldError errors={[errors.dbParameterGroupFamily]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="parameter-group-description">Description</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.description}
            id="parameter-group-description"
            placeholder="Tuned settings for the orders database"
            {...register("description")}
          />
          <FieldError errors={[errors.description]} />
        </Field>

        <TagsFieldArray control={control} name="tags" />

        <p className="text-xs text-muted-foreground">
          A new group starts empty: every value is the engine default until you
          override one, so a fresh group and the default group behave
          identically.
        </p>

        <CliCommandPanel
          commands={buildCreateParameterGroupCommands({
            dbParameterGroupName: values.dbParameterGroupName ?? "",
            dbParameterGroupFamily: values.dbParameterGroupFamily ?? "",
            description: values.description ?? "",
          })}
        />

        <FormActions
          error={createGroup.error}
          errorMsg="Failed to create the DB parameter group"
          isPending={createGroup.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Parameter Group"
        />
      </FormLock>
    </form>
  )
}

interface ParameterGroupCliValues {
  dbParameterGroupName: string
  dbParameterGroupFamily: string
  description: string
}

function buildCreateParameterGroupCommands(
  values: ParameterGroupCliValues,
): CliCommand[] {
  return [
    {
      label: "Create DB Parameter Group",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws rds create-db-parameter-group",
        },
        ...commandFlag(
          "--db-parameter-group-name",
          cliPlaceholder(values.dbParameterGroupName, "DBParameterGroupName"),
        ),
        ...commandFlag(
          "--db-parameter-group-family",
          cliPlaceholder(
            values.dbParameterGroupFamily,
            "DBParameterGroupFamily",
          ),
        ),
        ...commandFlag(
          "--description",
          `"${cliPlaceholder(values.description, "Description")}"`,
        ),
      ],
    },
  ]
}
