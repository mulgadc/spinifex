import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it } from "vitest"

import { TagPills, TruncatedText } from "./table-cells"

const TAGS = [
  { Key: "Name", Value: "web-1" },
  { Key: "spinifex:eks-nodegroup", Value: "node-gap" },
  { Key: "spinifex:eks-cluster", Value: "test-cluster" },
]

describe("TagPills", () => {
  it("shows one pill and counts the rest, leaving out omitted keys", () => {
    render(<TagPills omit={["Name"]} tags={TAGS} />)
    expect(screen.getByText("spinifex:eks-cluster")).toBeInTheDocument()
    expect(screen.getByText("+1")).toBeInTheDocument()
    expect(screen.queryByText("Name")).not.toBeInTheDocument()
  })

  it("lists every tag with its value on hover", async () => {
    render(<TagPills omit={["Name"]} tags={TAGS} />)
    await userEvent.hover(screen.getByText("spinifex:eks-cluster"))
    await expect(screen.findByText("test-cluster")).resolves.toBeInTheDocument()
    expect(screen.getByText("node-gap")).toBeInTheDocument()
  })

  it("shows a dash when nothing is left to show", () => {
    render(<TagPills omit={["Name"]} tags={[{ Key: "Name", Value: "x" }]} />)
    expect(screen.getByText("—")).toBeInTheDocument()
  })
})

describe("TruncatedText", () => {
  it("shows the full text on hover", async () => {
    const long = "a-very-long-instance-name-that-would-widen-its-column"
    render(<TruncatedText text={long} />)
    await userEvent.hover(screen.getByText(long))
    await expect(screen.findAllByText(long)).resolves.toHaveLength(2)
  })
})
