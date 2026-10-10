import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { DetailCard } from "./detail-card"

describe("DetailCard", () => {
  it("renders children", () => {
    render(<DetailCard>Card content</DetailCard>)
    expect(screen.getByText("Card content")).toBeInTheDocument()
  })

  it("renders header", () => {
    render(<DetailCard.Header>Section Title</DetailCard.Header>)
    expect(
      screen.getByRole("heading", { name: "Section Title" }),
    ).toBeInTheDocument()
  })

  it("renders content", () => {
    render(<DetailCard.Content>Inner content</DetailCard.Content>)
    expect(screen.getByText("Inner content")).toBeInTheDocument()
  })

  it("composes root, header, and content together", () => {
    render(
      <DetailCard>
        <DetailCard.Header>Title</DetailCard.Header>
        <DetailCard.Content>Body</DetailCard.Content>
      </DetailCard>,
    )
    expect(screen.getByRole("heading", { name: "Title" })).toBeInTheDocument()
    expect(screen.getByText("Body")).toBeInTheDocument()
  })
})
