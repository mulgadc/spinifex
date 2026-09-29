import { render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { ListCard } from "./list-card"

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    className,
    ...rest
  }: {
    children: React.ReactNode
    className?: string
    to?: string
  }) => (
    <a className={className} href={rest.to}>
      {children}
    </a>
  ),
}))

describe("ListCard", () => {
  it("renders the title", () => {
    render(<ListCard title="my-vpc" to={"/vpcs/123" as never} />)
    expect(screen.getByText("my-vpc")).toBeInTheDocument()
  })

  it("renders subtitle when provided", () => {
    render(
      <ListCard
        title="my-vpc"
        subtitle="vpc-abc123"
        to={"/vpcs/123" as never}
      />,
    )
    expect(screen.getByText("vpc-abc123")).toBeInTheDocument()
  })

  it("does not render subtitle when not provided", () => {
    const { container } = render(
      <ListCard title="my-vpc" to={"/vpcs/123" as never} />,
    )
    const paragraphs = container.querySelectorAll("p")
    expect(paragraphs).toHaveLength(0)
  })

  it("renders badge when provided", () => {
    render(
      <ListCard
        title="my-vpc"
        badge={<span>running</span>}
        to={"/vpcs/123" as never}
      />,
    )
    expect(screen.getByText("running")).toBeInTheDocument()
  })

  it("renders children", () => {
    render(
      <ListCard title="my-vpc" to={"/vpcs/123" as never}>
        <span>extra info</span>
      </ListCard>,
    )
    expect(screen.getByText("extra info")).toBeInTheDocument()
  })

  it("passes link props through", () => {
    render(<ListCard title="my-vpc" to={"/vpcs/123" as never} />)
    const link = screen.getByRole("link")
    expect(link).toHaveAttribute("href", "/vpcs/123")
  })
})
