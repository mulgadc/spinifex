import type { QueryClient } from "@tanstack/react-query"
import { fireEvent, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CreateDBInstancePage } from "./create-db-instance-page"

const ENGINE_VERSIONS = [
  {
    Engine: "postgres",
    EngineVersion: "18",
    DBParameterGroupFamily: "postgres18",
    DBEngineVersionDescription: "PostgreSQL 18",
  },
  {
    Engine: "mariadb",
    EngineVersion: "11.8",
    DBParameterGroupFamily: "mariadb11.8",
    DBEngineVersionDescription: "MariaDB 11.8",
  },
]

function image(
  engine: string,
  version = engine === "postgres" ? "18" : "11.8",
  withDataContract = true,
) {
  return {
    ImageId: `ami-${engine}`,
    Name: `spinifex-rds-${engine}`,
    Tags: [
      { Key: "spinifex:managed-by", Value: "rds" },
      { Key: "engine", Value: engine },
      { Key: "engine-version", Value: version },
      ...(withDataContract
        ? [
            {
              Key: "rds-data-volume-contract",
              Value: "format-auth-v1",
            },
          ]
        : []),
    ],
  }
}

function seed(images: unknown[]): QueryClient {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "engineVersions"], {
    DBEngineVersions: ENGINE_VERSIONS,
  })
  qc.setQueryData(["rds", "subnetGroups"], {
    DBSubnetGroups: [
      { DBSubnetGroupName: "db-subnets", VpcId: "vpc-1" },
      { DBSubnetGroupName: "db-subnets-2", VpcId: "vpc-2" },
    ],
  })
  qc.setQueryData(["rds", "parameterGroups"], {
    DBParameterGroups: [
      {
        DBParameterGroupName: "custom-pg",
        DBParameterGroupFamily: "postgres18",
      },
    ],
  })
  qc.setQueryData(["ec2", "securityGroups"], {
    SecurityGroups: [
      { GroupId: "sg-1", GroupName: "default", VpcId: "vpc-1" },
      { GroupId: "sg-2", GroupName: "default", VpcId: "vpc-2" },
    ],
  })
  qc.setQueryData(["ec2", "vpcs"], {
    Vpcs: [
      { VpcId: "vpc-1", IsDefault: true },
      { VpcId: "vpc-2", IsDefault: false },
    ],
  })
  qc.setQueryData(["ec2", "images"], { Images: images })

  for (const engine of ["postgres", "mariadb"]) {
    qc.setQueryData(["rds", "orderableOptions", engine], {
      OrderableDBInstanceOptions: [
        {
          Engine: engine,
          DBInstanceClass: "db.t3.micro",
          MinStorageSize: 20,
          MaxStorageSize: 65_536,
        },
        { Engine: engine, DBInstanceClass: "db.m5.large" },
      ],
    })
  }

  return qc
}

function renderForm() {
  return renderWithClient(
    <CreateDBInstancePage onClose={vi.fn()} />,
    seed([image("postgres"), image("mariadb")]),
  )
}

async function openAdvanced(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "Advanced settings" }))
}

async function choose(
  user: ReturnType<typeof userEvent.setup>,
  trigger: string,
  option: string,
) {
  await user.click(screen.getByLabelText(trigger))
  await user.click(screen.getByRole("option", { name: option }))
}

// Each picker opens a long list, which is slow under a loaded test run.
const SLOW_FORM_MS = 15_000

// Pasted rather than typed: every keystroke re-renders the whole form.
async function enter(
  user: ReturnType<typeof userEvent.setup>,
  label: string,
  text: string,
) {
  await user.click(screen.getByLabelText(label))
  await user.paste(text)
}

async function fillRequired(user: ReturnType<typeof userEvent.setup>) {
  await enter(user, "DB instance identifier", "orders-db")
  await choose(user, "DB instance class", "db.t3.micro")
  await enter(user, "Master username", "dbadmin")
  await enter(user, "Master password", "Sup3rSecretPw")
  await enter(user, "Confirm master password", "Sup3rSecretPw")
}

describe("CreateDBInstancePage system image gating", () => {
  it("renders the full form when both engine images are imported", () => {
    renderWithClient(
      <CreateDBInstancePage onClose={vi.fn()} />,
      seed([image("postgres"), image("mariadb")]),
    )
    expect(screen.getByLabelText("DB instance identifier")).toBeInTheDocument()
    expect(screen.queryByText(/image not found/)).toBeNull()
    expect(
      screen.getByRole("button", { name: "Create Database" }),
    ).toBeEnabled()
  })

  it("keeps the form usable when only one engine image is imported", () => {
    renderWithClient(
      <CreateDBInstancePage onClose={vi.fn()} />,
      seed([image("postgres")]),
    )
    expect(screen.getByLabelText("DB instance identifier")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create Database" }),
    ).toBeEnabled()
    expect(screen.queryByText(/RDS system image not found/)).toBeNull()
  })

  it("keeps the engine without an image visible but unselectable", () => {
    renderWithClient(
      <CreateDBInstancePage onClose={vi.fn()} />,
      seed([image("postgres")]),
    )

    expect(screen.getByRole("radio", { name: "PostgreSQL" })).toBeEnabled()
    expect(screen.getByRole("radio", { name: "MariaDB" })).toBeDisabled()
    expect(screen.getByText("Image not imported")).toBeInTheDocument()
  })

  it("shows each engine as a tile with its own mark", () => {
    const { container } = renderForm()

    expect(
      [...container.querySelectorAll<HTMLImageElement>("img[data-engine]")].map(
        (img) => img.dataset.engine,
      ),
    ).toStrictEqual(["postgres", "mariadb"])
  })

  it("puts the engine before the identifier", () => {
    const { container } = renderForm()
    const inputs = [...container.querySelectorAll("input")]

    expect(
      inputs.indexOf(screen.getByRole("radio", { name: "PostgreSQL" })),
    ).toBeLessThan(
      inputs.indexOf(screen.getByLabelText("DB instance identifier")),
    )
  })

  it("defaults to an engine that can actually boot", () => {
    renderWithClient(
      <CreateDBInstancePage onClose={vi.fn()} />,
      seed([image("mariadb")]),
    )
    expect(screen.getByRole("radio", { name: "MariaDB" })).toBeChecked()
    expect(screen.getByRole("radio", { name: "PostgreSQL" })).not.toBeChecked()
  })

  it("carries the import command for the engine that has no image", () => {
    renderWithClient(
      <CreateDBInstancePage onClose={vi.fn()} />,
      seed([image("postgres")]),
    )
    expect(screen.getByText("MariaDB image not found")).toBeInTheDocument()
    expect(
      screen.getByText(/spx admin images import.*mariadb/),
    ).toBeInTheDocument()
    expect(screen.queryByText("PostgreSQL image not found")).toBeNull()
  })

  it("rejects an image for a different engine version", () => {
    renderWithClient(
      <CreateDBInstancePage onClose={vi.fn()} />,
      seed([image("postgres", "17"), image("mariadb")]),
    )

    expect(screen.getByRole("radio", { name: "PostgreSQL" })).toBeDisabled()
  })

  it("rejects an image without the data-volume contract", () => {
    renderWithClient(
      <CreateDBInstancePage onClose={vi.fn()} />,
      seed([image("postgres", "18", false), image("mariadb")]),
    )

    expect(screen.getByRole("radio", { name: "PostgreSQL" })).toBeDisabled()
  })

  it("replaces the form when no engine image is imported", () => {
    renderWithClient(<CreateDBInstancePage onClose={vi.fn()} />, seed([]))
    expect(screen.getByText("RDS system image not found")).toBeInTheDocument()
    expect(screen.queryByLabelText("DB instance identifier")).toBeNull()
    expect(screen.getByText(/spx admin images import/)).toBeInTheDocument()
  })
})

describe("CreateDBInstancePage fields", () => {
  it.each([
    "DB instance identifier",
    "Engine version",
    "DB instance class",
    "Allocated storage (GiB)",
    "Master username",
    "Master password",
    "Confirm master password",
    "Initial database name",
    "Port",
    "DB subnet group",
  ])("offers the %s field", (label) => {
    renderForm()
    expect(screen.getByLabelText(label)).toBeInTheDocument()
  })

  it.each([
    "DB parameter group",
    "Backup retention (days)",
    "Backup window (UTC)",
    "Maintenance window (UTC)",
  ])("keeps %s in Advanced settings, closed by default", async (label) => {
    const user = userEvent.setup()
    renderForm()
    expect(screen.queryByLabelText(label)).toBeNull()

    await openAdvanced(user)
    expect(screen.getByLabelText(label)).toBeInTheDocument()
  })

  it.each(["Engine", "Settings", "Instance configuration", "Connectivity"])(
    "groups the form under a %s section",
    (title) => {
      renderForm()
      expect(screen.getByRole("button", { name: title })).toHaveAttribute(
        "aria-expanded",
        "true",
      )
    },
  )

  it.each([
    "Storage encryption",
    "Not available on Spinifex",
    "Multi-AZ deployment",
    "Public access",
    "Storage autoscaling",
  ])("drops the %s note", (text) => {
    renderForm()
    expect(screen.queryByText(new RegExp(text, "i"))).toBeNull()
    expect(screen.queryByRole("switch", { name: text })).toBeNull()
  })

  it.each([
    "IAM database authentication",
    "Provisioned IOPS",
    "Storage throughput",
    "Availability zone",
    "DB cluster identifier",
    "CloudWatch",
    "Auto minor version upgrade",
    "Copy tags to snapshot",
    "Monitoring",
    "Performance Insights",
    "Option group",
    "License model",
  ])("renders no live control for %s", (label) => {
    renderForm()
    expect(screen.queryByLabelText(new RegExp(label, "i"))).toBeNull()
  })

  it("leaves deletion protection as the only live setting switch", () => {
    renderForm()

    const enabled = screen
      .getAllByRole("switch")
      .filter((toggle) => toggle.getAttribute("aria-disabled") !== "true")
      .filter(
        (toggle) =>
          !toggle.getAttribute("aria-label")?.startsWith("Security group"),
      )

    expect(
      enabled.map((toggle) => toggle.getAttribute("aria-label")),
    ).toStrictEqual(["Enable deletion protection"])
  })

  it("lists the security groups as switches, with no checkboxes", () => {
    renderForm()
    expect(
      screen.getAllByRole("switch", { name: /^Security group / }),
    ).toHaveLength(1)
    expect(screen.queryAllByRole("checkbox")).toHaveLength(0)
  })

  it("offers security groups only from the default placement VPC", () => {
    renderForm()
    expect(
      screen.getByLabelText("Security group sg-1 (default)"),
    ).toBeInTheDocument()
    expect(screen.queryByLabelText("Security group sg-2 (default)")).toBeNull()
  })

  it("clears incompatible security groups when placement changes", async () => {
    const user = userEvent.setup()
    renderForm()
    await user.click(screen.getByLabelText("Security group sg-1 (default)"))

    await user.click(screen.getByLabelText("DB subnet group"))
    await user.click(
      screen.getByRole("option", { name: "db-subnets-2 (vpc-2)" }),
    )
    expect(
      screen.getByLabelText("Security group sg-2 (default)"),
    ).not.toBeChecked()

    await user.click(screen.getByLabelText("DB subnet group"))
    await user.click(screen.getByRole("option", { name: "db-subnets (vpc-1)" }))
    expect(
      screen.getByLabelText("Security group sg-1 (default)"),
    ).not.toBeChecked()
  })

  it("offers only the instance classes the orderable catalog lists", () => {
    renderForm()
    expect(screen.getByLabelText("DB instance class")).toBeInTheDocument()
    expect(screen.queryByText("db.r5.large")).toBeNull()
  })

  it("states the storage bounds the catalog reported in its tip", async () => {
    const user = userEvent.setup()
    renderForm()
    expect(screen.queryByText(/This engine accepts/)).toBeNull()

    await user.click(
      screen.getByRole("button", { name: "About Allocated storage (GiB)" }),
    )
    await expect(
      screen.findByText(/This engine accepts 20–65536 GiB/),
    ).resolves.toBeInTheDocument()
  })

  it("builds a CLI command that never echoes the password", () => {
    renderForm()
    fireEvent.click(screen.getByRole("button", { name: "AWS CLI" }))
    expect(screen.getByText(/\$DB_PASSWORD/)).toBeInTheDocument()
    expect(screen.getByText(/create-db-instance/)).toBeInTheDocument()
  })

  it("names the parameters a blank field has not filled in yet", () => {
    renderForm()
    fireEvent.click(screen.getByRole("button", { name: "AWS CLI" }))
    expect(screen.getByText(/<DBInstanceIdentifier>/)).toBeInTheDocument()
  })
})

describe("CreateDBInstancePage submit", () => {
  it("shows a refused create beside the button and stays open", async () => {
    mockSend.mockRejectedValueOnce(
      Object.assign(new Error("DB instance orders-db already exists"), {
        name: "DBInstanceAlreadyExists",
      }),
    )
    const onClose = vi.fn()
    const user = userEvent.setup()
    renderWithClient(
      <CreateDBInstancePage onClose={onClose} />,
      seed([image("postgres"), image("mariadb")]),
    )

    await fillRequired(user)
    await user.click(screen.getByRole("button", { name: "Create Database" }))

    await expect(
      screen.findByText(
        "DBInstanceAlreadyExists: DB instance orders-db already exists",
      ),
    ).resolves.toBeInTheDocument()
    expect(mockSend.mock.calls[0]?.[0].input.DBInstanceIdentifier).toBe(
      "orders-db",
    )
    expect(onClose).not.toHaveBeenCalled()
    expect(
      screen.getByRole("button", { name: "Create Database" }),
    ).toBeEnabled()
  })

  it(
    "sends the windows the pickers built",
    async () => {
      const user = userEvent.setup()
      renderForm()
      await fillRequired(user)
      await openAdvanced(user)
      await choose(user, "Backup window start", "03:00")
      await choose(user, "Maintenance window day", "Saturday")
      await choose(user, "Maintenance window start", "23:30")
      await user.click(screen.getByRole("button", { name: "Create Database" }))

      await vi.waitFor(() => {
        expect(mockSend).toHaveBeenCalled()
      })
      expect(mockSend.mock.lastCall?.[0].input).toMatchObject({
        PreferredBackupWindow: "03:00-03:30",
        PreferredMaintenanceWindow: "sat:23:30-sun:00:00",
      })
    },
    SLOW_FORM_MS,
  )

  it(
    "opens Advanced settings on an overlap and names it beside the field",
    async () => {
      const user = userEvent.setup()
      renderForm()
      await fillRequired(user)
      await openAdvanced(user)
      await choose(user, "Backup window start", "03:00")
      await choose(user, "Maintenance window day", "Thursday")
      await choose(user, "Maintenance window start", "03:00")
      await openAdvanced(user)
      expect(screen.queryByLabelText("Maintenance window (UTC)")).toBeNull()

      mockSend.mockClear()
      await user.click(screen.getByRole("button", { name: "Create Database" }))

      await expect(
        screen.findByText(
          "The maintenance window overlaps the backup window at that time of day",
        ),
      ).resolves.toBeInTheDocument()
      expect(
        screen.getByLabelText("Maintenance window (UTC)"),
      ).toBeInTheDocument()
      expect(mockSend).not.toHaveBeenCalledWith(
        expect.objectContaining({
          input: expect.objectContaining({ DBInstanceIdentifier: "orders-db" }),
        }),
      )
    },
    SLOW_FORM_MS,
  )
})
