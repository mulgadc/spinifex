import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { WizardFailureBanner } from "./wizard-failure-banner"

describe("WizardFailureBanner", () => {
  it("renders nothing for a wizard run that succeeded", () => {
    const { container } = render(
      <WizardFailureBanner
        result={{ created: [{ type: "VPC", id: "vpc-1" }] }}
      />,
    )

    expect(container).toBeEmptyDOMElement()
  })

  it("names the failed step and lists what was left behind", () => {
    render(
      <WizardFailureBanner
        createdHeading="Left behind:"
        result={{
          created: [
            { type: "VPC", id: "vpc-1" },
            { type: "Listener", id: undefined },
          ],
          error: new Error("quota exceeded"),
          failedStep: "creating subnet",
        }}
      />,
    )

    expect(
      screen.getByRole("heading", { name: "Wizard failed: creating subnet" }),
    ).toBeInTheDocument()
    expect(screen.getByText("quota exceeded")).toBeInTheDocument()
    expect(screen.getByText("Left behind:")).toBeInTheDocument()
    expect(
      screen.getAllByRole("listitem").map((li) => li.textContent),
    ).toStrictEqual(["VPC: vpc-1", "Listener: (created)"])
  })

  it("omits the leftover list when the first step failed", () => {
    render(
      <WizardFailureBanner
        result={{
          created: [],
          error: new Error("denied"),
          failedStep: "creating VPC",
        }}
      />,
    )

    expect(screen.queryByRole("list")).not.toBeInTheDocument()
  })
})
