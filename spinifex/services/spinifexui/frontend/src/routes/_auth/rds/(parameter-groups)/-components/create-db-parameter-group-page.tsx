import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Plus, Trash2 } from "lucide-react"
import { Controller, useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
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
    getValues,
    handleSubmit,
    register,
    setValue,
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

        <Field>
          <FieldTitle>Tags</FieldTitle>
          <div className="space-y-2">
            {(values.tags ?? []).map((_, index) => (
              // oxlint-disable-next-line react/no-array-index-key -- form array with no stable id
              <div className="flex items-center gap-2" key={index}>
                <Input placeholder="Key" {...register(`tags.${index}.key`)} />
                <Input
                  placeholder="Value"
                  {...register(`tags.${index}.value`)}
                />
                <Button
                  onClick={() =>
                    setValue(
                      "tags",
                      getValues("tags").filter((__, i) => i !== index),
                    )
                  }
                  size="icon"
                  type="button"
                  variant="ghost"
                >
                  <Trash2 className="size-3.5" />
                </Button>
              </div>
            ))}
            <Button
              onClick={() =>
                setValue("tags", [...getValues("tags"), { key: "", value: "" }])
              }
              size="sm"
              type="button"
              variant="outline"
            >
              <Plus className="size-3.5" />
              Add tag
            </Button>
          </div>
        </Field>

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

// An empty field reads as the parameter's name in angle brackets, so the
// command stays copyable and obviously incomplete rather than malformed.
function placeholder(value: string, name: string): string {
  return value.length > 0 ? value : `<${name}>`
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
        { type: "flag", value: " --db-parameter-group-name" },
        {
          type: "value",
          value: ` ${placeholder(values.dbParameterGroupName, "DBParameterGroupName")}`,
        },
        { type: "flag", value: " --db-parameter-group-family" },
        {
          type: "value",
          value: ` ${placeholder(values.dbParameterGroupFamily, "DBParameterGroupFamily")}`,
        },
        { type: "flag", value: " --description" },
        {
          type: "value",
          value: ` "${placeholder(values.description, "Description")}"`,
        },
      ],
    },
  ]
}
