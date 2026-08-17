import { z } from "zod"

// Mirrors handlers/rds/validate.go and handlers/rds/engine.go so the form
// rejects what the backend would reject, before the round trip. Engines,
// versions and instance classes are deliberately absent: those are read from
// DescribeDBEngineVersions and DescribeOrderableDBInstanceOptions.

export const MIN_ALLOCATED_STORAGE_GIB = 20
export const MAX_ALLOCATED_STORAGE_GIB = 65_536

export const MIN_DB_PORT = 1150
export const MAX_DB_PORT = 65_535

export const MIN_BACKUP_RETENTION_DAYS = 0
export const MAX_BACKUP_RETENTION_DAYS = 7

const MAX_IDENTIFIER_LEN = 63
const MIN_PASSWORD_LEN = 8
const MAX_PASSWORD_LEN = 128

// The window granularity and shortest window AWS accepts, in minutes.
const MIN_WINDOW_MINUTES = 30
const MINUTES_PER_DAY = 24 * 60

// The engine rules that no describe action reports. Everything else about an
// engine comes from the catalog queries; these live here because the API has
// nowhere to publish them.
interface EngineRules {
  reservedUsernames: string[]
  reservedUsernamePrefixes: string[]
  maxUsernameLen: number
  maxDBNameLen: number
}

const ENGINE_RULES: Record<string, EngineRules> = {
  postgres: {
    reservedUsernames: ["rdsadmin", "postgres", "rds_superuser"],
    reservedUsernamePrefixes: ["pg_"],
    maxUsernameLen: 63,
    maxDBNameLen: 63,
  },
  mariadb: {
    reservedUsernames: ["root", "mariadb.sys", "mysql", "rdsadmin", "public"],
    reservedUsernamePrefixes: ["mysql.", "mariadb."],
    maxUsernameLen: 80,
    maxDBNameLen: 64,
  },
}

// An engine the catalog offers but this table does not know gets the loosest
// rules rather than a hard failure: the backend still checks, and blocking a
// newly added engine in the console would be worse than a late error.
const FALLBACK_ENGINE_RULES: EngineRules = {
  reservedUsernames: [],
  reservedUsernamePrefixes: [],
  maxUsernameLen: 80,
  maxDBNameLen: 64,
}

export function engineRules(engine: string): EngineRules {
  return ENGINE_RULES[engine] ?? FALLBACK_ENGINE_RULES
}

// Lowercase letters, digits and hyphens, opening on a letter, with no trailing
// or doubled hyphen. The identifier is also the endpoint hostname, which is
// what makes it a DNS label rather than a free-form name.
export const dbInstanceIdentifierField = z
  .string()
  .min(1, "Identifier is required")
  .max(
    MAX_IDENTIFIER_LEN,
    `Identifier must be at most ${MAX_IDENTIFIER_LEN} characters`,
  )
  .regex(/^[a-z]/, "Identifier must begin with a lowercase letter")
  .regex(
    /^[a-z0-9-]*$/,
    "Identifier may contain only lowercase letters, digits and hyphens",
  )
  .refine((v) => !v.endsWith("-"), "Identifier may not end with a hyphen")
  .refine(
    (v) => !v.includes("--"),
    "Identifier may not contain consecutive hyphens",
  )

// Printable ASCII only, minus the four characters that would not survive the
// bootstrap handoff or the connection strings built from it.
export const masterPasswordField = z
  .string()
  .min(
    MIN_PASSWORD_LEN,
    `Password must be at least ${MIN_PASSWORD_LEN} characters`,
  )
  .max(
    MAX_PASSWORD_LEN,
    `Password must be at most ${MAX_PASSWORD_LEN} characters`,
  )
  .regex(/^[ -~]*$/, "Password may only contain printable ASCII")
  .refine(
    (v) => !/[/"@ ]/.test(v),
    "Password may not contain '/', '\"', '@' or spaces",
  )

const allocatedStorageField = z
  .number()
  .int("Storage must be a whole number of GiB")
  .min(
    MIN_ALLOCATED_STORAGE_GIB,
    `Storage must be at least ${MIN_ALLOCATED_STORAGE_GIB} GiB`,
  )
  .max(
    MAX_ALLOCATED_STORAGE_GIB,
    `Storage must be at most ${MAX_ALLOCATED_STORAGE_GIB} GiB`,
  )

// A string rather than a number so blank stays distinct from zero: the port
// defaults to the engine's own, and no describe action publishes what that is,
// so an empty field is the only honest way to ask for it.
const portField = z.string().refine((v) => {
  if (v === "") {
    return true
  }
  const port = Number(v)
  return (
    /^\d+$/.test(v) &&
    Number.isInteger(port) &&
    port >= MIN_DB_PORT &&
    port <= MAX_DB_PORT
  )
}, `Port must be between ${MIN_DB_PORT} and ${MAX_DB_PORT}`)

const backupRetentionField = z
  .number()
  .int("Retention must be a whole number of days")
  .min(MIN_BACKUP_RETENTION_DAYS, "Retention may not be negative")
  .max(
    MAX_BACKUP_RETENTION_DAYS,
    `Retention must be at most ${MAX_BACKUP_RETENTION_DAYS} days`,
  )

const DAILY_WINDOW_REGEX =
  /^(?:[01]\d|2[0-3]):[0-5]\d-(?:[01]\d|2[0-3]):[0-5]\d$/
const WEEKLY_WINDOW_REGEX =
  /^(?:sun|mon|tue|wed|thu|fri|sat):(?:[01]\d|2[0-3]):[0-5]\d-(?:sun|mon|tue|wed|thu|fri|sat):(?:[01]\d|2[0-3]):[0-5]\d$/

const WEEKDAYS: string[] = ["sun", "mon", "tue", "wed", "thu", "fri", "sat"]

function clockMinutes(value: string): number {
  const [hours, minutes] = value.split(":")
  return Number(hours ?? 0) * 60 + Number(minutes ?? 0)
}

function weekdayClockMinutes(value: string): number {
  const [day, hours, minutes] = value.split(":")
  const index = WEEKDAYS.indexOf(day ?? "")
  return (
    index * MINUTES_PER_DAY + Number(hours ?? 0) * 60 + Number(minutes ?? 0)
  )
}

// A window that wraps is measured forward through the end of the period, so an
// equal start and end reads as zero rather than a whole period.
function windowLength(start: number, end: number, period: number): number {
  return (((end - start) % period) + period) % period
}

export function dailyWindowLength(value: string): number {
  const [from, to] = value.split("-")
  return windowLength(
    clockMinutes(from ?? ""),
    clockMinutes(to ?? ""),
    MINUTES_PER_DAY,
  )
}

export function weeklyWindowLength(value: string): number {
  const [from, to] = value.split("-")
  return windowLength(
    weekdayClockMinutes(from ?? ""),
    weekdayClockMinutes(to ?? ""),
    7 * MINUTES_PER_DAY,
  )
}

// Both windows are optional: an unnamed one is assigned deterministically from
// the identifier, and an empty string is how the form says "leave it unset".
const preferredBackupWindowField = z
  .string()
  .refine(
    (v) => v === "" || DAILY_WINDOW_REGEX.test(v),
    "Backup window must be hh24:mi-hh24:mi in UTC",
  )
  .refine(
    (v) => v === "" || dailyWindowLength(v) >= MIN_WINDOW_MINUTES,
    `Backup window must be at least ${MIN_WINDOW_MINUTES} minutes long`,
  )

const preferredMaintenanceWindowField = z
  .string()
  .refine(
    (v) => v === "" || WEEKLY_WINDOW_REGEX.test(v),
    "Maintenance window must be ddd:hh24:mi-ddd:hh24:mi in UTC",
  )
  .refine(
    (v) => v === "" || weeklyWindowLength(v) >= MIN_WINDOW_MINUTES,
    `Maintenance window must be at least ${MIN_WINDOW_MINUTES} minutes long`,
  )

export const rdsTagSchema = z.object({
  key: z.string().min(1, "Tag key is required").max(128),
  value: z.string().max(256),
})

export type RdsTagFormData = z.infer<typeof rdsTagSchema>

// The shared identifier rule for a master username and an initial database
// name: letters, digits and underscores, opening on a letter. Narrower than
// either engine accepts, because rds-init interpolates both into the guest.
function checkEngineIdentifier(
  ctx: z.RefinementCtx,
  path: string,
  field: string,
  value: string,
  maxLen: number,
) {
  const issue = (message: string) =>
    ctx.addIssue({ code: "custom", path: [path], message })

  if (value.length > maxLen) {
    issue(`${field} must be at most ${maxLen} characters`)
    return
  }
  if (!/^[a-zA-Z]/.test(value)) {
    issue(`${field} must begin with a letter`)
    return
  }
  if (!/^\w*$/.test(value)) {
    issue(`${field} may contain only letters, digits and underscores`)
  }
}

function checkMasterUsername(
  ctx: z.RefinementCtx,
  engine: string,
  username: string,
) {
  const rules = engineRules(engine)
  checkEngineIdentifier(
    ctx,
    "masterUsername",
    "Master username",
    username,
    rules.maxUsernameLen,
  )

  const lower = username.trim().toLowerCase()
  if (rules.reservedUsernames.includes(lower)) {
    ctx.addIssue({
      code: "custom",
      path: ["masterUsername"],
      message: `"${username}" is reserved by ${engine}`,
    })
    return
  }
  const prefix = rules.reservedUsernamePrefixes.find((p) => lower.startsWith(p))
  if (prefix) {
    ctx.addIssue({
      code: "custom",
      path: ["masterUsername"],
      message: `Master username may not begin with "${prefix}", which ${engine} reserves`,
    })
  }
}

export const createDBInstanceSchema = z
  .object({
    dbInstanceIdentifier: dbInstanceIdentifierField,
    engine: z.string().min(1, "Engine is required"),
    engineVersion: z.string(),
    dbInstanceClass: z.string().min(1, "Instance class is required"),
    allocatedStorage: allocatedStorageField,
    masterUsername: z.string().min(1, "Master username is required"),
    masterUserPassword: masterPasswordField,
    confirmPassword: z.string(),
    dbName: z.string(),
    port: portField,
    dbSubnetGroupName: z.string(),
    vpcSecurityGroupIds: z.array(z.string()),
    dbParameterGroupName: z.string(),
    deletionProtection: z.boolean(),
    backupRetentionPeriod: backupRetentionField,
    preferredBackupWindow: preferredBackupWindowField,
    preferredMaintenanceWindow: preferredMaintenanceWindowField,
    tags: z.array(rdsTagSchema),
  })
  .superRefine((data, ctx) => {
    checkMasterUsername(ctx, data.engine, data.masterUsername)

    if (data.dbName !== "") {
      checkEngineIdentifier(
        ctx,
        "dbName",
        "Database name",
        data.dbName,
        engineRules(data.engine).maxDBNameLen,
      )
    }
    if (data.confirmPassword !== data.masterUserPassword) {
      ctx.addIssue({
        code: "custom",
        path: ["confirmPassword"],
        message: "Passwords do not match",
      })
    }
  })

export type CreateDBInstanceFormData = z.infer<typeof createDBInstanceSchema>

// Storage grows only, so the floor is the instance's current size rather than
// the platform minimum. currentAllocatedStorage carries it into the refinement.
export const modifyDBInstanceSchema = z
  .object({
    currentAllocatedStorage: z.number(),
    dbInstanceClass: z.string().min(1, "Instance class is required"),
    allocatedStorage: allocatedStorageField,
    dbParameterGroupName: z.string(),
    vpcSecurityGroupIds: z.array(z.string()),
    deletionProtection: z.boolean(),
    backupRetentionPeriod: backupRetentionField,
    preferredBackupWindow: preferredBackupWindowField,
    preferredMaintenanceWindow: preferredMaintenanceWindowField,
    masterUserPassword: z.string(),
    applyImmediately: z.boolean(),
  })
  .superRefine((data, ctx) => {
    if (data.allocatedStorage < data.currentAllocatedStorage) {
      ctx.addIssue({
        code: "custom",
        path: ["allocatedStorage"],
        message: `Storage can only grow; it is currently ${data.currentAllocatedStorage} GiB`,
      })
    }
    // Empty means "leave the password alone", so the character rules only
    // apply once the field carries something.
    if (data.masterUserPassword !== "") {
      const result = masterPasswordField.safeParse(data.masterUserPassword)
      if (!result.success) {
        ctx.addIssue({
          code: "custom",
          path: ["masterUserPassword"],
          message: result.error.issues[0]?.message ?? "Password is invalid",
        })
      }
    }
  })

export type ModifyDBInstanceFormData = z.infer<typeof modifyDBInstanceSchema>

// Statuses handlers/rds/status.go treats as in-flight. The list and detail
// queries poll while any instance is in one and stop once none is.
export const TRANSITIONAL_DB_INSTANCE_STATUSES = new Set([
  "creating",
  "modifying",
  "backing-up",
  "rebooting",
  "stopping",
  "starting",
  "recovering",
  "deleting",
])

export function isTransitionalStatus(status: string | undefined): boolean {
  return status !== undefined && TRANSITIONAL_DB_INSTANCE_STATUSES.has(status)
}

// The lifecycle actions each settled status permits, from the transition table
// in handlers/rds/status.go. Anything else leaves every action disabled rather
// than offering one the backend would refuse.
export function canStart(status: string | undefined): boolean {
  return status === "stopped"
}

export function canStop(status: string | undefined): boolean {
  return status === "available"
}

export function canReboot(status: string | undefined): boolean {
  return status === "available"
}

export function canDelete(status: string | undefined): boolean {
  return status !== undefined && status !== "deleting" && status !== "deleted"
}
