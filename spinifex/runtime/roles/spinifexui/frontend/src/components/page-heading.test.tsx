import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { PageHeading } from "./page-heading"

describe("PageHeading", () => {
  it("renders the title", () => {
    render(<PageHeading title="Instances" />)
    expect(screen.getByText("Instances")).toBeInTheDocument()
  })

  it("renders the subtitle when provided", () => {
    render(
      <PageHeading subtitle="Manage your EC2 instances" title="Instances" />,
    )
    expect(screen.getByText("Manage your EC2 instances")).toBeInTheDocument()
  })

  it("does not render a subtitle when not provided", () => {
    render(<PageHeading title="Instances" />)
    expect(
      screen.queryByText("Manage your EC2 instances"),
    ).not.toBeInTheDocument()
  })

  it("renders actions when provided", () => {
    render(
      <PageHeading
        actions={<button type="button">Launch</button>}
        title="Instances"
      />,
    )
    expect(screen.getByText("Launch")).toBeInTheDocument()
  })
})
