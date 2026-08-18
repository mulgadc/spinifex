import type { QueryClient } from "@tanstack/react-query"
import { fireEvent, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
  getEc2Client: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => vi.fn(),
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
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

describe("CreateDBInstancePage system image gating", () => {
  it("renders the full form when both engine images are imported", () => {
    renderWithClient(
      <CreateDBInstancePage />,
      seed([image("postgres"), image("mariadb")]),
    )
    expect(screen.getByLabelText("DB instance identifier")).toBeInTheDocument()
    expect(screen.queryByText(/image not found/)).toBeNull()
    expect(
      screen.getByRole("button", { name: "Create Database" }),
    ).toBeEnabled()
  })

  it("keeps the form usable when only one engine image is imported", () => {
    renderWithClient(<CreateDBInstancePage />, seed([image("postgres")]))
    expect(screen.getByLabelText("DB instance identifier")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create Database" }),
    ).toBeEnabled()
    expect(screen.queryByText(/RDS system image not found/)).toBeNull()
  })

  it("keeps the engine without an image visible but unselectable", async () => {
    const user = userEvent.setup()
    renderWithClient(<CreateDBInstancePage />, seed([image("postgres")]))

    await user.click(screen.getByLabelText("Engine"))

    expect(screen.getByRole("option", { name: "postgres" })).toBeEnabled()
    const missing = screen.getByRole("option", {
      name: "mariadb — image not imported",
    })
    expect(missing).toHaveAttribute("aria-disabled", "true")
  })

  it("defaults to an engine that can actually boot", () => {
    renderWithClient(<CreateDBInstancePage />, seed([image("mariadb")]))
    expect(screen.getByLabelText("Engine")).toHaveTextContent("mariadb")
    expect(screen.queryByLabelText("DB instance identifier")).not.toBeNull()
  })

  it("carries the import command for the engine that has no image", () => {
    renderWithClient(<CreateDBInstancePage />, seed([image("postgres")]))
    expect(screen.getByText("mariadb image not found")).toBeInTheDocument()
    expect(
      screen.getByText(/spx admin images import.*mariadb/),
    ).toBeInTheDocument()
    expect(screen.queryByText("postgres image not found")).toBeNull()
  })

  it("rejects an image for a different engine version", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <CreateDBInstancePage />,
      seed([image("postgres", "17"), image("mariadb")]),
    )

    await user.click(screen.getByLabelText("Engine"))
    expect(
      screen.getByRole("option", { name: "postgres — image not imported" }),
    ).toHaveAttribute("aria-disabled", "true")
  })

  it("rejects an image without the data-volume contract", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <CreateDBInstancePage />,
      seed([image("postgres", "18", false), image("mariadb")]),
    )

    await user.click(screen.getByLabelText("Engine"))
    expect(
      screen.getByRole("option", { name: "postgres — image not imported" }),
    ).toHaveAttribute("aria-disabled", "true")
  })

  it("replaces the form when no engine image is imported", () => {
    renderWithClient(<CreateDBInstancePage />, seed([]))
    expect(screen.getByText("RDS system image not found")).toBeInTheDocument()
    expect(screen.queryByLabelText("DB instance identifier")).toBeNull()
    expect(screen.getByText(/spx admin images import/)).toBeInTheDocument()
  })
})

describe("CreateDBInstancePage fields", () => {
  function renderForm() {
    return renderWithClient(
      <CreateDBInstancePage />,
      seed([image("postgres"), image("mariadb")]),
    )
  }

  it.each([
    "DB instance identifier",
    "Engine",
    "Engine version",
    "DB instance class",
    "Allocated storage (GiB)",
    "Master username",
    "Master password",
    "Confirm master password",
    "Initial database name",
    "Port",
    "DB subnet group",
    "DB parameter group",
    "Backup retention (days)",
    "Preferred backup window",
    "Preferred maintenance window",
  ])("offers the %s field", (label) => {
    renderForm()
    expect(screen.getByLabelText(label)).toBeInTheDocument()
  })

  it("shows encryption as a fixed row rather than a choice", () => {
    renderForm()
    expect(screen.getByText("Encrypted — always on.")).toBeInTheDocument()
    expect(screen.queryByLabelText(/KMS/i)).toBeNull()
  })

  it.each(["Multi-AZ deployment", "Public access", "Storage autoscaling"])(
    "renders %s as a disabled control with a note",
    (label) => {
      renderForm()
      const control = screen.getByText(label).closest("label")
      expect(control?.querySelector("input")).toBeDisabled()
      expect(control).toHaveTextContent("Not available on Spinifex")
    },
  )

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

  it("leaves deletion protection and security groups as the only live checkboxes", () => {
    renderForm()
    const enabled = screen
      .getAllByRole("checkbox")
      .filter((box) => !(box as HTMLInputElement).disabled)
    expect(enabled).toHaveLength(2)
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

  it("states the storage bounds the catalog reported", () => {
    renderForm()
    expect(
      screen.getByText(/This engine accepts 20–65536 GiB/),
    ).toBeInTheDocument()
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
