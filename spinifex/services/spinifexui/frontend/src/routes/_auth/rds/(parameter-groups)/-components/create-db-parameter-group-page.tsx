import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Controller, useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  cliPlaceholder,
  commandFlag,
  type CliCommand,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
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

import { TagsFieldArray } from "../../-components/tags-field-array"

export function CreateDBParameterGroupPage() {
  const navigate = useNavigate()
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
    await createGroup.mutateAsync(data)
    await navigate({
      to: "/rds/describe-db-parameter-groups/$name",
      params: { name: data.dbParameterGroupName },
    })
  }

  return (
    <>
      <BackLink to="/rds/describe-db-parameter-groups">
        Back to parameter groups
      </BackLink>
      <PageHeading title="Create DB parameter group" />

      {createGroup.error && (
        <ErrorBanner
          error={createGroup.error}
          msg="Failed to create the DB parameter group"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
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
          isPending={createGroup.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/rds/describe-db-parameter-groups" })
          }
          pendingLabel="Creating…"
          submitLabel="Create parameter group"
        />
      </form>
    </>
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
