import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it } from "vitest"

import {
  CliCommandPanel,
  partsToText,
  type CliCommand,
} from "./cli-command-panel"

const singleCommand: CliCommand[] = [
  {
    label: "Create Key Pair",
    parts: [
      { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-key-pair" },
      { type: "flag", value: " --key-name" },
      { type: "value", value: " my-key" },
    ],
  },
]

const multiCommand: CliCommand[] = [
  {
    label: "Create VPC",
    parts: [
      { type: "variable", value: "VPC_ID=" },
      { type: "bin", value: "$(AWS_PROFILE=spinifex aws ec2 create-vpc" },
      { type: "flag", value: " --cidr-block" },
      { type: "value", value: " 10.0.0.0/16" },
      { type: "flag", value: " --query" },
      { type: "value", value: " 'Vpc.VpcId' --output text)" },
    ],
  },
  {
    label: "Create Subnet",
    parts: [
      { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-subnet" },
      { type: "flag", value: " --vpc-id" },
      { type: "variable", value: ' "$VPC_ID"' },
      { type: "flag", value: " --cidr-block" },
      { type: "value", value: " 10.0.1.0/24" },
    ],
  },
]

describe("CliCommandPanel", () => {
  it("renders nothing when commands array is empty", () => {
    const { container } = render(<CliCommandPanel commands={[]} />)
    expect(container.firstChild).toBeNull()
  })

  it("renders collapsed by default", () => {
    render(<CliCommandPanel commands={singleCommand} />)
    expect(screen.getByText("AWS CLI")).toBeInTheDocument()
    expect(screen.queryByText("Copy")).not.toBeInTheDocument()
  })

  it("expands on click and shows command", async () => {
    const user = userEvent.setup()
    render(<CliCommandPanel commands={singleCommand} />)

    await user.click(screen.getByText("AWS CLI"))

    expect(screen.getByText("Copy")).toBeInTheDocument()
    expect(screen.getByText("my-key")).toBeInTheDocument()
  })

  it("collapses on second click", async () => {
    const user = userEvent.setup()
    render(<CliCommandPanel commands={singleCommand} />)

    await user.click(screen.getByText("AWS CLI"))
    expect(screen.getByText("Copy")).toBeInTheDocument()

    await user.click(screen.getByText("AWS CLI"))
    expect(screen.queryByText("Copy")).not.toBeInTheDocument()
  })

  it("sets aria-expanded correctly", async () => {
    const user = userEvent.setup()
    render(<CliCommandPanel commands={singleCommand} />)

    const toggle = screen.getByRole("button", { name: /aws cli/i })
    expect(toggle).toHaveAttribute("aria-expanded", "false")

    await user.click(toggle)
    expect(toggle).toHaveAttribute("aria-expanded", "true")
  })

  it("applies correct styles to each part type", async () => {
    const user = userEvent.setup()
    render(<CliCommandPanel commands={singleCommand} />)
    await user.click(screen.getByText("AWS CLI"))

    const binPart = screen.getByText(
      "AWS_PROFILE=spinifex aws ec2 create-key-pair",
    )
    expect(binPart.className).toContain("text-tactical-cyan")
    expect(binPart.className).toContain("font-semibold")

    const flagPart = screen.getByText("--key-name", { exact: false })
    expect(flagPart.className).toContain("text-tactical-amber")

    const valuePart = screen.getByText("my-key", { exact: false })
    expect(valuePart.className).toContain("text-foreground")
  })

  it("applies correct styles to variable and comment parts", async () => {
    const user = userEvent.setup()
    const commands: CliCommand[] = [
      {
        label: "Test",
        parts: [
          { type: "comment", value: "# test comment\n" },
          { type: "variable", value: "MY_VAR=" },
          { type: "value", value: "hello" },
        ],
      },
    ]
    render(<CliCommandPanel commands={commands} />)
    await user.click(screen.getByText("AWS CLI"))

    const commentPart = screen.getByText("# test comment")
    expect(commentPart.className).toContain("text-muted-foreground")
    expect(commentPart.className).toContain("italic")

    const varPart = screen.getByText("MY_VAR=")
    expect(varPart.className).toContain("text-tactical-green")
  })

  it("copies plain text to clipboard", async () => {
    const user = userEvent.setup()
    render(<CliCommandPanel commands={singleCommand} />)
    await user.click(screen.getByText("AWS CLI"))
    await user.click(screen.getByText("Copy"))

    const clipText = await navigator.clipboard.readText()
    expect(clipText).toBe(
      "AWS_PROFILE=spinifex aws ec2 create-key-pair --key-name my-key",
    )
  })

  it("shows command count for multi-command", async () => {
    const user = userEvent.setup()
    render(<CliCommandPanel commands={multiCommand} />)
    await user.click(screen.getByText("AWS CLI"))

    expect(screen.getByText("2 commands")).toBeInTheDocument()
  })

  it("shows labels for each command in multi-command mode", async () => {
    const user = userEvent.setup()
    render(<CliCommandPanel commands={multiCommand} />)
    await user.click(screen.getByText("AWS CLI"))

    expect(screen.getByText("Create VPC")).toBeInTheDocument()
    expect(screen.getByText("Create Subnet")).toBeInTheDocument()
  })

  it("does not show label in single-command mode", async () => {
    const user = userEvent.setup()
    render(<CliCommandPanel commands={singleCommand} />)
    await user.click(screen.getByText("AWS CLI"))

    expect(screen.queryByText("Create Key Pair")).not.toBeInTheDocument()
  })

  it("shows 'command' text for single command", async () => {
    const user = userEvent.setup()
    render(<CliCommandPanel commands={singleCommand} />)
    await user.click(screen.getByText("AWS CLI"))

    expect(screen.getByText("command")).toBeInTheDocument()
  })
})

describe("partsToText", () => {
  it("flattens single command to plain text", () => {
    expect(partsToText(singleCommand)).toBe(
      "AWS_PROFILE=spinifex aws ec2 create-key-pair --key-name my-key",
    )
  })

  it("joins multiple commands with double newlines", () => {
    const result = partsToText(multiCommand)
    expect(result).toContain("create-vpc")
    expect(result).toContain("\n\n")
    expect(result).toContain("create-subnet")
  })
})
