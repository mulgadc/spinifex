import { describe, expect, it } from "vitest"

import {
  type CreateDBInstanceFormData,
  canDelete,
  canDeleteSnapshot,
  canReboot,
  canRestoreSnapshot,
  canSnapshot,
  canStart,
  canStop,
  applyMethodsFor,
  createDBInstanceSchema,
  createDBParameterGroupSchema,
  createDBSubnetGroupSchema,
  dailyWindowLength,
  dbInstanceIdentifierField,
  dbSnapshotIdentifierField,
  isDefaultParameterGroupName,
  isTransitionalStatus,
  MAX_TAGS_PER_RESOURCE,
  masterPasswordField,
  modifyDBInstanceSchema,
  restoreDBInstanceSchema,
  suggestedIdentifier,
  weeklyWindowLength,
} from "./rds"

const VALID_CREATE: CreateDBInstanceFormData = {
  dbInstanceIdentifier: "orders-db",
  engine: "postgres",
  engineVersion: "18",
  dbInstanceClass: "db.t3.micro",
  allocatedStorage: 20,
  masterUsername: "dbadmin",
  masterUserPassword: "sup3rsecret",
  confirmPassword: "sup3rsecret",
  dbName: "orders",
  port: "",
  dbSubnetGroupName: "",
  vpcSecurityGroupIds: [],
  dbParameterGroupName: "",
  deletionProtection: false,
  backupRetentionPeriod: 7,
  preferredBackupWindow: "",
  preferredMaintenanceWindow: "",
  tags: [],
}

function createWith(
  overrides: Partial<CreateDBInstanceFormData>,
): ReturnType<typeof createDBInstanceSchema.safeParse> {
  return createDBInstanceSchema.safeParse({ ...VALID_CREATE, ...overrides })
}

describe("tags", () => {
  it.each(["aws:cost-centre", "AWS:CostCentre", "Aws:owner"])(
    "refuses the reserved prefix in %s, as validateTagKey does",
    (key) => {
      const result = createWith({ tags: [{ key, value: "x" }] })
      expect(result.success).toBeFalsy()
    },
  )

  it("accepts a key that merely contains aws: further in", () => {
    const result = createWith({ tags: [{ key: "team-aws:owner", value: "x" }] })
    expect(result.success).toBeTruthy()
  })

  it("accepts the maximum number of tags and refuses one more", () => {
    const atCap = Array.from({ length: MAX_TAGS_PER_RESOURCE }, (_, i) => ({
      key: `k${i}`,
      value: "v",
    }))
    expect(createWith({ tags: atCap }).success).toBeTruthy()
    expect(
      createWith({ tags: [...atCap, { key: "extra", value: "v" }] }).success,
    ).toBeFalsy()
  })
})

describe("dbInstanceIdentifierField", () => {
  it("accepts a lowercase DNS-label identifier", () => {
    expect(
      dbInstanceIdentifierField.safeParse("orders-db-1").success,
    ).toBeTruthy()
  })

  it.each([
    ["empty", ""],
    ["opening on a digit", "1orders"],
    ["opening on a hyphen", "-orders"],
    ["uppercase", "OrdersDB"],
    ["underscored", "orders_db"],
    ["trailing hyphen", "orders-"],
    ["doubled hyphen", "orders--db"],
    ["over 63 characters", "a".repeat(64)],
  ])("rejects an identifier %s", (_name, value) => {
    expect(dbInstanceIdentifierField.safeParse(value).success).toBeFalsy()
  })
})

describe("masterPasswordField", () => {
  it("accepts eight printable ASCII characters", () => {
    expect(masterPasswordField.safeParse("abcd1234").success).toBeTruthy()
  })

  it.each([
    ["too short", "abc1234"],
    ["too long", "a".repeat(129)],
    ["a slash", "abcd/1234"],
    ["a double quote", 'abcd"1234'],
    ["an at sign", "abcd@1234"],
    ["a space", "abcd 1234"],
    ["a tab", "abcd\t1234"],
    ["a non-ASCII character", "abcd€1234"],
  ])("rejects a password containing %s", (_name, value) => {
    expect(masterPasswordField.safeParse(value).success).toBeFalsy()
  })
})

describe("createDBInstanceSchema", () => {
  it("accepts a well-formed request", () => {
    expect(createWith({}).success).toBeTruthy()
  })

  it("rejects mismatched password confirmation", () => {
    expect(
      createWith({ confirmPassword: "something-else" }).success,
    ).toBeFalsy()
  })

  it.each(["rdsadmin", "postgres", "rds_superuser", "PostgreS"])(
    "rejects the postgres reserved username %s",
    (username) => {
      expect(createWith({ masterUsername: username }).success).toBeFalsy()
    },
  )

  it("rejects a postgres username with the pg_ prefix", () => {
    expect(createWith({ masterUsername: "pg_owner" }).success).toBeFalsy()
  })

  it.each(["root", "mysql", "rdsadmin", "public"])(
    "rejects the mariadb reserved username %s",
    (username) => {
      expect(
        createWith({ engine: "mariadb", masterUsername: username }).success,
      ).toBeFalsy()
    },
  )

  it("accepts postgres-reserved names on mariadb and vice versa", () => {
    expect(
      createWith({ engine: "mariadb", masterUsername: "postgres" }).success,
    ).toBeTruthy()
    expect(
      createWith({ engine: "postgres", masterUsername: "root" }).success,
    ).toBeTruthy()
  })

  it("applies each engine's own username length limit", () => {
    const name = `a${"b".repeat(70)}`
    expect(createWith({ masterUsername: name }).success).toBeFalsy()
    expect(
      createWith({ engine: "mariadb", masterUsername: name }).success,
    ).toBeTruthy()
  })

  it.each([
    ["opening on a digit", "1admin"],
    ["hyphenated", "db-admin"],
    ["dotted", "db.admin"],
  ])("rejects a master username %s", (_name, username) => {
    expect(createWith({ masterUsername: username }).success).toBeFalsy()
  })

  it("treats a blank database name as creating no database", () => {
    expect(createWith({ dbName: "" }).success).toBeTruthy()
  })

  it("rejects a database name that is not an engine identifier", () => {
    expect(createWith({ dbName: "orders-db" }).success).toBeFalsy()
    expect(createWith({ dbName: "1orders" }).success).toBeFalsy()
  })

  it.each([19, 65_537])("rejects allocated storage of %i GiB", (storage) => {
    expect(createWith({ allocatedStorage: storage }).success).toBeFalsy()
  })

  it.each([20, 65_536])("accepts allocated storage of %i GiB", (storage) => {
    expect(createWith({ allocatedStorage: storage }).success).toBeTruthy()
  })

  it.each(["1149", "65536", "0", "abc"])("rejects the port %s", (port) => {
    expect(createWith({ port }).success).toBeFalsy()
  })

  it.each(["1150", "5432", "65535", ""])("accepts the port %s", (port) => {
    expect(createWith({ port }).success).toBeTruthy()
  })

  it.each([-1, 8])("rejects a retention of %i days", (days) => {
    expect(createWith({ backupRetentionPeriod: days }).success).toBeFalsy()
  })

  it.each([0, 7])("accepts a retention of %i days", (days) => {
    expect(createWith({ backupRetentionPeriod: days }).success).toBeTruthy()
  })

  it.each([
    ["03:00-03:30", true],
    ["03:00-03:29", false],
    ["23:45-00:15", true],
    ["03:00-03:00", false],
    ["3:00-3:30", false],
    ["24:00-24:30", false],
    ["03:00", false],
  ])("validates the backup window %s", (window, valid) => {
    expect(createWith({ preferredBackupWindow: window }).success).toBe(valid)
  })

  it.each([
    ["sun:03:00-sun:03:30", true],
    ["sat:23:45-sun:00:15", true],
    ["sun:03:00-sun:03:15", false],
    ["xyz:03:00-sun:03:30", false],
    ["03:00-03:30", false],
  ])("validates the maintenance window %s", (window, valid) => {
    expect(createWith({ preferredMaintenanceWindow: window }).success).toBe(
      valid,
    )
  })
})

describe("window length", () => {
  it("measures a wrapping daily window forward through midnight", () => {
    expect(dailyWindowLength("23:45-00:15")).toBe(30)
  })

  it("reads an equal start and end as zero rather than a whole day", () => {
    expect(dailyWindowLength("03:00-03:00")).toBe(0)
  })

  it("measures a wrapping weekly window forward through the week", () => {
    expect(weeklyWindowLength("sat:23:45-sun:00:15")).toBe(30)
  })
})

describe("modifyDBInstanceSchema", () => {
  const VALID_MODIFY = {
    currentAllocatedStorage: 40,
    dbInstanceClass: "db.t3.micro",
    allocatedStorage: 40,
    dbParameterGroupName: "",
    vpcSecurityGroupIds: [],
    deletionProtection: false,
    backupRetentionPeriod: 7,
    preferredBackupWindow: "",
    preferredMaintenanceWindow: "",
    masterUserPassword: "",
    applyImmediately: false,
  }

  it("accepts leaving storage unchanged", () => {
    expect(modifyDBInstanceSchema.safeParse(VALID_MODIFY).success).toBeTruthy()
  })

  it("accepts growing storage", () => {
    expect(
      modifyDBInstanceSchema.safeParse({
        ...VALID_MODIFY,
        allocatedStorage: 80,
      }).success,
    ).toBeTruthy()
  })

  it("rejects shrinking storage below the current size", () => {
    expect(
      modifyDBInstanceSchema.safeParse({
        ...VALID_MODIFY,
        allocatedStorage: 30,
      }).success,
    ).toBeFalsy()
  })

  it("treats a blank password as leaving it alone", () => {
    expect(
      modifyDBInstanceSchema.safeParse({
        ...VALID_MODIFY,
        masterUserPassword: "",
      }).success,
    ).toBeTruthy()
  })

  it("applies the password rules once a password is supplied", () => {
    expect(
      modifyDBInstanceSchema.safeParse({
        ...VALID_MODIFY,
        masterUserPassword: "short",
      }).success,
    ).toBeFalsy()
    expect(
      modifyDBInstanceSchema.safeParse({
        ...VALID_MODIFY,
        masterUserPassword: "abcd@1234",
      }).success,
    ).toBeFalsy()
  })
})

describe("status helpers", () => {
  it.each([
    "creating",
    "modifying",
    "backing-up",
    "rebooting",
    "stopping",
    "starting",
    "recovering",
    "deleting",
  ])("treats %s as in flight", (status) => {
    expect(isTransitionalStatus(status)).toBeTruthy()
  })

  it.each(["available", "stopped", "failed", "deleted", undefined])(
    "treats %s as settled",
    (status) => {
      expect(isTransitionalStatus(status)).toBeFalsy()
    },
  )

  it("permits only the transitions the backend allows", () => {
    expect(canStart("stopped")).toBeTruthy()
    expect(canStart("available")).toBeFalsy()
    expect(canStop("available")).toBeTruthy()
    expect(canStop("stopped")).toBeFalsy()
    expect(canReboot("available")).toBeTruthy()
    expect(canReboot("stopped")).toBeFalsy()
    expect(canDelete("available")).toBeTruthy()
    expect(canDelete("stopped")).toBeTruthy()
    expect(canDelete("deleting")).toBeFalsy()
    expect(canDelete("deleted")).toBeFalsy()
  })
})

const VALID_SUBNET_GROUP = {
  dbSubnetGroupName: "orders-subnets",
  dbSubnetGroupDescription: "Private subnets for the orders database",
  subnetIds: ["subnet-1", "subnet-2"],
  tags: [],
}

const VALID_PARAMETER_GROUP = {
  dbParameterGroupName: "orders-postgres18",
  dbParameterGroupFamily: "postgres18",
  description: "Tuned settings",
  tags: [],
}

// Mirrors validateDBGroupName, which both group kinds share.
describe("group name rules", () => {
  it("accepts a well-formed subnet group", () => {
    expect(
      createDBSubnetGroupSchema.safeParse(VALID_SUBNET_GROUP).success,
    ).toBeTruthy()
  })

  it.each([
    ["", "empty"],
    ["1orders", "opening on a digit"],
    ["-orders", "opening on a hyphen"],
    ["orders.subnets", "holding a full stop"],
    ["orders_subnets", "holding an underscore"],
    ["default", "the reserved name"],
    ["DEFAULT", "the reserved name in another case"],
    ["a".repeat(256), "longer than 255 characters"],
  ])("rejects %s (%s)", (name) => {
    expect(
      createDBSubnetGroupSchema.safeParse({
        ...VALID_SUBNET_GROUP,
        dbSubnetGroupName: name,
      }).success,
    ).toBeFalsy()
  })

  it("requires a description, as the backend does", () => {
    expect(
      createDBSubnetGroupSchema.safeParse({
        ...VALID_SUBNET_GROUP,
        dbSubnetGroupDescription: "",
      }).success,
    ).toBeFalsy()
  })

  it("requires at least one subnet and at most twenty", () => {
    expect(
      createDBSubnetGroupSchema.safeParse({
        ...VALID_SUBNET_GROUP,
        subnetIds: [],
      }).success,
    ).toBeFalsy()
    expect(
      createDBSubnetGroupSchema.safeParse({
        ...VALID_SUBNET_GROUP,
        subnetIds: Array.from({ length: 21 }, (_, i) => `subnet-${i}`),
      }).success,
    ).toBeFalsy()
  })
})

describe("parameter group schema", () => {
  it("accepts a well-formed group", () => {
    expect(
      createDBParameterGroupSchema.safeParse(VALID_PARAMETER_GROUP).success,
    ).toBeTruthy()
  })

  it("names the reservation when the default prefix is used", () => {
    const result = createDBParameterGroupSchema.safeParse({
      ...VALID_PARAMETER_GROUP,
      dbParameterGroupName: "default.postgres18",
    })
    expect(result.success).toBeFalsy()
    expect(result.error?.issues[0]?.message).toContain("reserves")
  })

  it("requires a family", () => {
    expect(
      createDBParameterGroupSchema.safeParse({
        ...VALID_PARAMETER_GROUP,
        dbParameterGroupFamily: "",
      }).success,
    ).toBeFalsy()
  })
})

describe("isDefaultParameterGroupName", () => {
  it.each(["default.postgres18", "default.mariadb11.8", "DEFAULT.postgres18"])(
    "treats %s as a default group",
    (name) => {
      expect(isDefaultParameterGroupName(name)).toBeTruthy()
    },
  )

  it.each(["orders-pg", "defaults-pg", "default", undefined])(
    "treats %s as a customer group",
    (name) => {
      expect(isDefaultParameterGroupName(name)).toBeFalsy()
    },
  )
})

// resolveApplyMethod rejects "immediate" on a static parameter rather than
// downgrading it, so the editor must not offer the choice.
describe("applyMethodsFor", () => {
  it("pins a static parameter to pending-reboot", () => {
    expect(applyMethodsFor("static")).toStrictEqual(["pending-reboot"])
  })

  it("offers both methods on a dynamic parameter", () => {
    expect(applyMethodsFor("dynamic")).toStrictEqual([
      "immediate",
      "pending-reboot",
    ])
  })
})

// Mirrors validateDBSnapshotIdentifier and validateDBSnapshotName in
// handlers/rds/snapshot.go.
describe("dbSnapshotIdentifierField", () => {
  it.each(["orders-db-snapshot-20260817-1432", "nightly", "a1"])(
    "accepts %s",
    (name) => {
      expect(dbSnapshotIdentifierField.safeParse(name).success).toBeTruthy()
    },
  )

  it.each([
    ["", "empty"],
    ["1nightly", "opening on a digit"],
    ["Nightly", "uppercase"],
    ["nightly-", "a trailing hyphen"],
    ["nightly--copy", "consecutive hyphens"],
    ["nightly_copy", "an underscore"],
  ])("rejects %s (%s)", (name) => {
    expect(dbSnapshotIdentifierField.safeParse(name).success).toBeFalsy()
  })

  it("rejects the rds: namespace by name rather than by punctuation", () => {
    const result = dbSnapshotIdentifierField.safeParse("rds:nightly")
    expect(result.success).toBeFalsy()
    expect(result.error?.issues[0]?.message).toContain("automated backups")
  })
})

describe("restoreDBInstanceSchema", () => {
  const VALID_RESTORE = {
    snapshotAllocatedStorage: 20,
    dbInstanceIdentifier: "orders-db-restored",
    dbInstanceClass: "db.t3.micro",
    allocatedStorage: 20,
    port: "",
    dbSubnetGroupName: "",
    vpcSecurityGroupIds: [],
    dbParameterGroupName: "",
    deletionProtection: false,
    tags: [],
  }

  it("accepts a restore onto the snapshot's own size", () => {
    expect(
      restoreDBInstanceSchema.safeParse(VALID_RESTORE).success,
    ).toBeTruthy()
  })

  it("accepts a restore that grows the volume", () => {
    const result = restoreDBInstanceSchema.safeParse({
      ...VALID_RESTORE,
      allocatedStorage: 40,
    })
    expect(result.success).toBeTruthy()
  })

  // CreateVolume refuses a size below the snapshot's, and a shrink has nowhere
  // to put the data the snapshot already holds.
  it("refuses to restore below the snapshot's size", () => {
    const result = restoreDBInstanceSchema.safeParse({
      ...VALID_RESTORE,
      snapshotAllocatedStorage: 40,
      allocatedStorage: 20,
    })
    expect(result.success).toBeFalsy()
    expect(result.error?.issues[0]?.message).toContain("40 GiB")
  })

  it("applies the DB instance identifier rules to the new instance", () => {
    const result = restoreDBInstanceSchema.safeParse({
      ...VALID_RESTORE,
      dbInstanceIdentifier: "Orders_Restored",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects a port outside the accepted range", () => {
    const result = restoreDBInstanceSchema.safeParse({
      ...VALID_RESTORE,
      port: "80",
    })
    expect(result.success).toBeFalsy()
  })
})

describe("snapshot action gating", () => {
  it.each(["available", "stopped"])(
    "allows a snapshot of a %s instance",
    (status) => {
      expect(canSnapshot(status)).toBeTruthy()
    },
  )

  it.each(["creating", "backing-up", "modifying", "deleting", undefined])(
    "refuses a snapshot of a %s instance",
    (status) => {
      expect(canSnapshot(status)).toBeFalsy()
    },
  )

  it("restores only from an available snapshot", () => {
    expect(canRestoreSnapshot("available")).toBeTruthy()
    expect(canRestoreSnapshot("creating")).toBeFalsy()
  })

  // DeleteDBSnapshot rejects the rds: namespace outright, so an automated
  // backup is never offered a delete however settled it is.
  it("never deletes an automated backup", () => {
    expect(canDeleteSnapshot("available", "manual")).toBeTruthy()
    expect(canDeleteSnapshot("available", "automated")).toBeFalsy()
    expect(canDeleteSnapshot("creating", "manual")).toBeFalsy()
    expect(canDeleteSnapshot("available", undefined)).toBeTruthy()
  })
})

describe("suggestedIdentifier", () => {
  const AT = new Date("2026-08-17T14:32:00Z")

  it("names the snapshot after the instance and the time", () => {
    expect(suggestedIdentifier("orders-db", "snapshot", AT)).toBe(
      "orders-db-snapshot-20260817-1432",
    )
  })

  it("trims a long instance name and leaves no trailing hyphen", () => {
    const long = `${"a".repeat(60)}-b`
    const result = suggestedIdentifier(long, "restored", AT)
    expect(result.length).toBeLessThanOrEqual(63)
    expect(dbInstanceIdentifierField.safeParse(result).success).toBeTruthy()
  })
})
