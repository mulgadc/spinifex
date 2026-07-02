import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { PushCommands } from "./push-commands"

const URI = "111.dkr.ecr.ap-southeast-2.local/team/app"

describe("PushCommands", () => {
  it("renders docker login/tag/push against the registry host", () => {
    render(<PushCommands repositoryName="team/app" repositoryUri={URI} />)
    const block = screen.getByText(/docker login/)
    expect(block.textContent).toContain(
      "docker login --username AWS --password-stdin 111.dkr.ecr.ap-southeast-2.local",
    )
    expect(block.textContent).toContain(`docker push ${URI}:latest`)
  })

  it("copies the commands to the clipboard", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText },
      configurable: true,
    })
    render(<PushCommands repositoryName="team/app" repositoryUri={URI} />)
    fireEvent.click(screen.getByRole("button", { name: "Copy" }))
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1))
    expect(writeText.mock.calls[0]![0]).toContain(`docker push ${URI}:latest`)
    expect(await screen.findByText("Copied")).toBeInTheDocument()
  })
})
