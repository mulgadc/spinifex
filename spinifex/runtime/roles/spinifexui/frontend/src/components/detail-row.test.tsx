import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { DetailRow } from "./detail-row"

describe("DetailRow", () => {
  it("renders label and value", () => {
    render(<DetailRow label="Instance ID" value="i-abc123" />)
    expect(screen.getByText("Instance ID")).toBeInTheDocument()
    expect(screen.getByText("i-abc123")).toBeInTheDocument()
  })

  it("renders dash when value is undefined", () => {
    render(<DetailRow label="Subnet" />)
    expect(screen.getByText("—")).toBeInTheDocument()
  })

  it("renders dash when value is empty string", () => {
    render(<DetailRow label="Subnet" value="" />)
    expect(screen.getByText("—")).toBeInTheDocument()
  })

  it("renders ReactNode as value", () => {
    render(<DetailRow label="Status" value={<span>running</span>} />)
    expect(screen.getByText("running")).toBeInTheDocument()
  })
})
