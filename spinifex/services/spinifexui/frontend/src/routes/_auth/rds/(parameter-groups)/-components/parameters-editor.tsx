import type { ApplyMethod, Parameter } from "@aws-sdk/client-rds"
import { useState } from "react"

import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useModifyDBParameterGroup } from "@/mutations/rds"
import {
  APPLY_METHOD_PENDING_REBOOT,
  APPLY_TYPE_STATIC,
  applyMethodsFor,
  MAX_PARAMETERS_PER_MODIFY,
  PARAMETER_SOURCE_USER,
} from "@/types/rds"

type SourceFilter = "all" | "user" | "engine-default"

interface Edit {
  value: string
  applyMethod: ApplyMethod
}

interface ParametersEditorProps {
  dbParameterGroupName: string
  parameters: Parameter[]
  // A default group has no stored record, so every value on it is fixed.
  readOnly: boolean
}

// The values a picker can offer, or null for a free-form field. A stored value
// outside the allowed set falls back to the text input rather than to a picker
// that cannot represent what is already there.
function optionsFor(parameter: Parameter): string[] | null {
  const allowed = parameter.AllowedValues ?? ""
  if (parameter.DataType !== "boolean" && parameter.DataType !== "enum") {
    return null
  }
  const options = allowed.split(",").filter((v) => v.length > 0)
  if (options.length === 0) {
    return null
  }
  return options.includes(parameter.ParameterValue ?? "") ? options : null
}

function isNumeric(parameter: Parameter): boolean {
  return parameter.DataType === "integer" || parameter.DataType === "real"
}

function defaultApplyMethod(parameter: Parameter): ApplyMethod {
  if (parameter.ApplyType === APPLY_TYPE_STATIC) {
    return APPLY_METHOD_PENDING_REBOOT
  }
  return parameter.ApplyMethod ?? APPLY_METHOD_PENDING_REBOOT
}

interface ParameterValueCellProps {
  parameter: Parameter
  value: string
  options: string[] | null
  editable: boolean
  onChange: (value: string) => void
}

// An unmodifiable parameter shows its value with no control at all: the backend
// refuses it by design, and hiding the row would read as the engine not
// offering the setting.
function ParameterValueCell({
  parameter,
  value,
  options,
  editable,
  onChange,
}: ParameterValueCellProps) {
  const name = parameter.ParameterName ?? ""
  if (!editable) {
    return <span className="font-mono text-xs">{value}</span>
  }
  if (options) {
    return (
      <Select onValueChange={(next) => onChange(next ?? "")} value={value}>
        <SelectTrigger aria-label={`Value of ${name}`} className="w-40">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {options.map((option) => (
            <SelectItem key={option} value={option}>
              {option}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    )
  }
  return (
    <Input
      aria-label={`Value of ${name}`}
      className="w-40"
      onChange={(e) => onChange(e.target.value)}
      type={isNumeric(parameter) ? "number" : "text"}
      value={value}
    />
  )
}

export function ParametersEditor({
  dbParameterGroupName,
  parameters,
  readOnly,
}: ParametersEditorProps) {
  const modifyGroup = useModifyDBParameterGroup()
  const [edits, setEdits] = useState<Record<string, Edit>>({})
  const [filterText, setFilterText] = useState("")
  const [sourceFilter, setSourceFilter] = useState<SourceFilter>("all")

  const editedNames = Object.keys(edits)
  const overLimit = editedNames.length > MAX_PARAMETERS_PER_MODIFY
  const hasBlankValue = editedNames.some(
    (name) => (edits[name]?.value ?? "").trim() === "",
  )

  const setEdit = (parameter: Parameter, next: Partial<Edit>) => {
    const name = parameter.ParameterName ?? ""
    setEdits((current) => ({
      ...current,
      [name]: {
        value: parameter.ParameterValue ?? "",
        applyMethod: defaultApplyMethod(parameter),
        ...current[name],
        ...next,
      },
    }))
  }

  const clearEdit = (name: string) => {
    setEdits((current) => {
      const { [name]: _removed, ...rest } = current
      return rest
    })
  }

  const handleSave = async () => {
    try {
      await modifyGroup.mutateAsync({
        dbParameterGroupName,
        parameters: editedNames.map((name) => ({
          name,
          value: edits[name]?.value ?? "",
          applyMethod: edits[name]?.applyMethod ?? APPLY_METHOD_PENDING_REBOOT,
        })),
      })
      setEdits({})
    } catch {
      // Surfaced below via modifyGroup.error
    }
  }

  const needle = filterText.trim().toLowerCase()
  const visible = parameters.filter((parameter) => {
    const source = parameter.Source ?? ""
    if (sourceFilter === "user" && source !== PARAMETER_SOURCE_USER) {
      return false
    }
    if (sourceFilter === "engine-default" && source === PARAMETER_SOURCE_USER) {
      return false
    }
    return (parameter.ParameterName ?? "").toLowerCase().includes(needle)
  })

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end gap-3">
        <div className="grow">
          <label className="sr-only" htmlFor="parameter-filter">
            Filter parameters
          </label>
          <Input
            id="parameter-filter"
            onChange={(e) => setFilterText(e.target.value)}
            placeholder="Filter by parameter name"
            value={filterText}
          />
        </div>
        <Select
          onValueChange={(value) => setSourceFilter(value ?? "all")}
          value={sourceFilter}
        >
          <SelectTrigger aria-label="Filter by source" className="w-56">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All parameters</SelectItem>
            <SelectItem value="user">Modified (user)</SelectItem>
            <SelectItem value="engine-default">Engine default</SelectItem>
          </SelectContent>
        </Select>
      </div>

      {!readOnly && editedNames.length > 0 && (
        <div className="flex flex-wrap items-center gap-3 rounded-md border p-3">
          <p className="text-sm">
            {editedNames.length} parameter
            {editedNames.length === 1 ? "" : "s"} edited
          </p>
          <Button
            disabled={overLimit || hasBlankValue || modifyGroup.isPending}
            onClick={() => void handleSave()}
            size="sm"
          >
            {modifyGroup.isPending ? "Saving…" : "Save changes"}
          </Button>
          <Button onClick={() => setEdits({})} size="sm" variant="outline">
            Discard
          </Button>
          {overLimit && (
            <p className="text-xs text-destructive">
              At most {MAX_PARAMETERS_PER_MODIFY} parameters can be saved at
              once. Each save is applied as a whole, so a larger edit is split
              by you rather than half-applied by us.
            </p>
          )}
          {hasBlankValue && (
            <p className="text-xs text-destructive">
              An edited parameter cannot be left blank.
            </p>
          )}
        </div>
      )}

      {modifyGroup.error && (
        <p className="text-sm text-destructive">{modifyGroup.error.message}</p>
      )}

      {visible.length > 0 ? (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 font-medium">Value</th>
                <th className="px-4 py-2 font-medium">Allowed</th>
                <th className="px-4 py-2 font-medium">Source</th>
                <th className="px-4 py-2 font-medium">Apply</th>
                <th className="px-4 py-2 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {visible.map((parameter) => {
                const name = parameter.ParameterName ?? ""
                const edit = edits[name]
                const options = optionsFor(parameter)
                const editable = !readOnly && parameter.IsModifiable
                const value = edit?.value ?? parameter.ParameterValue ?? ""
                const applyMethod =
                  edit?.applyMethod ?? defaultApplyMethod(parameter)
                const methods = applyMethodsFor(parameter.ApplyType)

                return (
                  <tr className="border-b align-top last:border-0" key={name}>
                    <td className="px-4 py-2 font-mono text-xs">
                      {name}
                      <span className="mt-0.5 block font-sans text-muted-foreground">
                        {parameter.Description}
                      </span>
                    </td>
                    <td className="px-4 py-2">
                      <ParameterValueCell
                        editable={editable ?? false}
                        onChange={(next) => setEdit(parameter, { value: next })}
                        options={options}
                        parameter={parameter}
                        value={value}
                      />
                    </td>
                    <td className="px-4 py-2 font-mono text-xs text-muted-foreground">
                      {parameter.AllowedValues ?? "—"}
                      <span className="mt-0.5 block font-sans">
                        {parameter.DataType}
                        {!parameter.IsModifiable && " · fixed by the platform"}
                      </span>
                    </td>
                    <td className="px-4 py-2 text-xs">{parameter.Source}</td>
                    <td className="px-4 py-2">
                      {editable && edit && methods.length > 1 ? (
                        <Select
                          onValueChange={(next) =>
                            setEdit(parameter, {
                              applyMethod: next ?? APPLY_METHOD_PENDING_REBOOT,
                            })
                          }
                          value={applyMethod}
                        >
                          <SelectTrigger
                            aria-label={`Apply method of ${name}`}
                            className="w-44"
                          >
                            <SelectValue />
                          </SelectTrigger>
                          <SelectContent>
                            {methods.map((method) => (
                              <SelectItem key={method} value={method}>
                                {method}
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                      ) : (
                        <span className="text-xs">
                          {applyMethod}
                          {parameter.ApplyType === APPLY_TYPE_STATIC && (
                            <span className="mt-0.5 block text-muted-foreground">
                              static — adopted at the next reboot
                            </span>
                          )}
                        </span>
                      )}
                    </td>
                    <td className="px-4 py-2 text-right">
                      {edit && (
                        <Button
                          onClick={() => clearEdit(name)}
                          size="sm"
                          variant="ghost"
                        >
                          Reset
                        </Button>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="text-muted-foreground">
          No parameter matches this filter.
        </p>
      )}
    </div>
  )
}
