import {
  type Control,
  type FieldValues,
  type Path,
  Controller,
} from "react-hook-form"

import { FieldLabel } from "@/components/form-fields"
import { Field, FieldError } from "@/components/ui/field"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  DURATIONS,
  formatClock,
  formatDailyWindow,
  formatDuration,
  formatWeeklyWindow,
  parseDailyWindow,
  parseWeeklyWindow,
  START_TIMES,
  WEEKDAY_LABELS,
  WEEKDAYS,
  withCurrent,
} from "@/lib/rds-windows"

// The base-ui Select needs a non-empty value to show an item's label.
const NONE = "none"

interface Item {
  value: string
  label: string
}

interface PartSelectProps {
  id?: string
  label: string
  items: Item[]
  value: string
  disabled?: boolean
  invalid?: boolean
  onChange: (value: string) => void
}

function PartSelect({
  id,
  label,
  items,
  value,
  disabled,
  invalid,
  onChange,
}: PartSelectProps) {
  return (
    <Select
      disabled={disabled}
      items={items}
      onValueChange={(next) => {
        onChange(next ?? NONE)
      }}
      value={value}
    >
      <SelectTrigger
        aria-invalid={invalid}
        aria-label={label}
        className="w-full"
        id={id}
      >
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {items.map((item) => (
          <SelectItem key={item.value} value={item.value}>
            {item.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

function startItems(current: number | undefined): Item[] {
  return withCurrent(START_TIMES, current).map((minutes) => ({
    value: String(minutes),
    label: formatClock(minutes),
  }))
}

function durationItems(current: number | undefined): Item[] {
  return withCurrent(DURATIONS, current).map((minutes) => ({
    value: String(minutes),
    label: formatDuration(minutes),
  }))
}

interface WindowFieldProps<T extends FieldValues> {
  control: Control<T>
  name: Path<T>
  id: string
}

// The daily backup window as a start time and a length, building AWS's
// "hh24:mi-hh24:mi". No preference leaves it unset so one is assigned.
export function BackupWindowField<T extends FieldValues>({
  control,
  name,
  id,
}: WindowFieldProps<T>) {
  return (
    <Controller
      control={control}
      name={name}
      render={({ field, fieldState }) => {
        const value: string =
          // oxlint-disable-next-line anti-slop/no-runtime-typeof -- the generic path's value has no narrower type to branch on
          typeof field.value === "string" ? field.value : ""

        const parsed = parseDailyWindow(value)
        const duration = parsed?.duration ?? DURATIONS[0] ?? 30

        return (
          <Field>
            <FieldLabel
              id={id}
              info="A daily snapshot of the database is taken in this window, in UTC. It must be at least 30 minutes long. With no preference, one is assigned clear of the maintenance window."
              label="Backup window (UTC)"
            />
            <div className="grid grid-cols-2 gap-2">
              <PartSelect
                id={id}
                invalid={!!fieldState.error}
                items={[
                  { value: NONE, label: "No preference" },
                  ...startItems(parsed?.start),
                ]}
                label="Backup window start"
                onChange={(next) => {
                  field.onChange(
                    next === NONE
                      ? ""
                      : formatDailyWindow({ start: Number(next), duration }),
                  )
                }}
                value={parsed ? String(parsed.start) : NONE}
              />
              <PartSelect
                disabled={!parsed}
                items={durationItems(parsed?.duration)}
                label="Backup window length"
                onChange={(next) => {
                  if (parsed) {
                    field.onChange(
                      formatDailyWindow({
                        start: parsed.start,
                        duration: Number(next),
                      }),
                    )
                  }
                }}
                value={String(duration)}
              />
            </div>
            <FieldError errors={[fieldState.error]} />
          </Field>
        )
      }}
    />
  )
}

const DAY_ITEMS: Item[] = [
  { value: NONE, label: "No preference" },
  ...WEEKDAYS.map((day, index) => ({
    value: String(index),
    label: WEEKDAY_LABELS[day],
  })),
]

// The weekly maintenance window as a day, a start time and a length, building
// AWS's "ddd:hh24:mi-ddd:hh24:mi"; one running past midnight ends the next day.
export function MaintenanceWindowField<T extends FieldValues>({
  control,
  name,
  id,
}: WindowFieldProps<T>) {
  return (
    <Controller
      control={control}
      name={name}
      render={({ field, fieldState }) => {
        const value: string =
          // oxlint-disable-next-line anti-slop/no-runtime-typeof -- the generic path's value has no narrower type to branch on
          typeof field.value === "string" ? field.value : ""

        const parsed = parseWeeklyWindow(value)
        const start = parsed?.start ?? 0
        const duration = parsed?.duration ?? DURATIONS[0] ?? 30

        const update = (next: Partial<{ day: number; start: number }>) => {
          field.onChange(
            formatWeeklyWindow({
              day: next.day ?? parsed?.day ?? 0,
              start: next.start ?? start,
              duration,
            }),
          )
        }

        return (
          <Field>
            <FieldLabel
              id={id}
              info="Patching and other changes that need a restart happen in this window, in UTC. It must be at least 30 minutes long and must not overlap the backup window at the same time of day. With no preference, one is assigned."
              label="Maintenance window (UTC)"
            />
            <div className="grid grid-cols-3 gap-2">
              <PartSelect
                id={id}
                invalid={!!fieldState.error}
                items={DAY_ITEMS}
                label="Maintenance window day"
                onChange={(next) => {
                  if (next === NONE) {
                    field.onChange("")
                  } else {
                    update({ day: Number(next) })
                  }
                }}
                value={parsed ? String(parsed.day) : NONE}
              />
              <PartSelect
                disabled={!parsed}
                items={startItems(parsed?.start)}
                label="Maintenance window start"
                onChange={(next) => {
                  update({ start: Number(next) })
                }}
                value={String(start)}
              />
              <PartSelect
                disabled={!parsed}
                items={durationItems(parsed?.duration)}
                label="Maintenance window length"
                onChange={(next) => {
                  if (parsed) {
                    field.onChange(
                      formatWeeklyWindow({ ...parsed, duration: Number(next) }),
                    )
                  }
                }}
                value={String(duration)}
              />
            </div>
            <FieldError errors={[fieldState.error]} />
          </Field>
        )
      }}
    />
  )
}
