import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { PolicyDocumentViewer } from "./policy-document-viewer"

describe("PolicyDocumentViewer", () => {
  it("formats valid JSON with indentation", () => {
    const doc = '{"Version":"2012-10-17","Statement":[]}'
    const { container } = render(<PolicyDocumentViewer document={doc} />)
    const pre = container.querySelector("pre")
    expect(pre?.textContent).toBe(JSON.stringify(JSON.parse(doc), null, 2))
  })

  it("renders invalid JSON as-is", () => {
    render(<PolicyDocumentViewer document="not valid json" />)
    expect(screen.getByText("not valid json")).toBeInTheDocument()
  })

  it("renders in a pre element", () => {
    render(<PolicyDocumentViewer document="{}" />)
    const pre = screen.getByText("{}").closest("pre")
    expect(pre).toBeInTheDocument()
  })
})
