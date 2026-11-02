import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { CreateInstanceProfilePage } from "./create-instance-profile-page"

const onClose = vi.fn()

function submit(name: string) {
  fireEvent.change(screen.getByLabelText("Instance Profile Name"), {
    target: { value: name },
  })
  fireEvent.click(
    screen.getByRole("button", { name: "Create Instance Profile" }),
  )
}

describe("CreateInstanceProfilePage", () => {
  it("creates it and closes with the new name", async () => {
    mockSend.mockResolvedValueOnce({
      InstanceProfile: { InstanceProfileName: "web-profile" },
    })
    renderWithClient(
      <CreateInstanceProfilePage onClose={onClose} />,
      createTestQueryClient(),
    )

    submit("web-profile")

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("web-profile")
    })
    expect(mockSend.mock.calls[0]?.[0].input.InstanceProfileName).toBe(
      "web-profile",
    )
  })

  it("shows a refused create beside the button and stays open", async () => {
    mockSend.mockRejectedValueOnce(
      Object.assign(new Error("Instance Profile web-profile already exists."), {
        name: "EntityAlreadyExists",
      }),
    )
    renderWithClient(
      <CreateInstanceProfilePage onClose={onClose} />,
      createTestQueryClient(),
    )

    submit("web-profile")

    await expect(
      screen.findByText(
        "EntityAlreadyExists: Instance Profile web-profile already exists.",
      ),
    ).resolves.toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
    expect(
      screen.getByRole("button", { name: "Create Instance Profile" }),
    ).toBeEnabled()
  })
})
