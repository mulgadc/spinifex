import { screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { renderWithProviders } from "@/test/utils"

import { InstanceActions } from "./instance-actions"

const TRANSITIONING_RE = /Actions will be available/
const TERMINATED_RE = /terminated and cannot be managed/
const TERMINATE_CONFIRM_RE = /Are you sure you want to terminate the instance/

// Mock mutation hooks
const mockMutate = vi.fn()
const defaultMutation = { mutate: mockMutate, isPending: false, error: null }

vi.mock("@/mutations/ec2", () => ({
  useStartInstance: () => ({ ...defaultMutation }),
  useStopInstance: () => ({ ...defaultMutation }),
  useRebootInstance: () => ({ ...defaultMutation }),
  useTerminateInstance: () => ({ ...defaultMutation }),
}))

beforeEach(() => {
  mockMutate.mockReset()
})

describe("InstanceActions", () => {
  describe("running instance", () => {
    it("shows Stop, Reboot, and Terminate buttons", () => {
      renderWithProviders(
        <InstanceActions instanceId="i-123" state="running" />,
      )
      expect(screen.getByText("Stop")).toBeInTheDocument()
      expect(screen.getByText("Reboot")).toBeInTheDocument()
      expect(screen.getByText("Terminate")).toBeInTheDocument()
      expect(screen.queryByText("Start")).not.toBeInTheDocument()
    })

    it("calls stop mutation on click", async () => {
      const user = userEvent.setup()
      renderWithProviders(
        <InstanceActions instanceId="i-123" state="running" />,
      )
      await user.click(screen.getByText("Stop"))
      expect(mockMutate).toHaveBeenCalledWith("i-123")
    })
  })

  describe("stopped instance", () => {
    it("shows Start and Terminate buttons", () => {
      renderWithProviders(
        <InstanceActions instanceId="i-123" state="stopped" />,
      )
      expect(screen.getByText("Start")).toBeInTheDocument()
      expect(screen.getByText("Terminate")).toBeInTheDocument()
      expect(screen.queryByText("Stop")).not.toBeInTheDocument()
      expect(screen.queryByText("Reboot")).not.toBeInTheDocument()
    })
  })

  describe("transitioning states", () => {
    it.each(["pending", "stopping", "shutting-down"])(
      "shows transitioning message for '%s' state",
      (state) => {
        renderWithProviders(
          <InstanceActions instanceId="i-123" state={state} />,
        )
        expect(screen.getByText(TRANSITIONING_RE)).toBeInTheDocument()
        expect(screen.queryByText("Start")).not.toBeInTheDocument()
        expect(screen.queryByText("Stop")).not.toBeInTheDocument()
      },
    )
  })

  describe("terminated instance", () => {
    it("shows terminated message", () => {
      renderWithProviders(
        <InstanceActions instanceId="i-123" state="terminated" />,
      )
      expect(screen.getByText(TERMINATED_RE)).toBeInTheDocument()
    })
  })

  describe("terminate confirmation", () => {
    it("does not call mutation when Terminate button is clicked", async () => {
      const user = userEvent.setup()
      renderWithProviders(
        <InstanceActions instanceId="i-123" state="running" />,
      )
      await user.click(screen.getByText("Terminate"))
      expect(mockMutate).not.toHaveBeenCalled()
    })

    it("opens confirmation dialog showing instance ID on Terminate click", async () => {
      const user = userEvent.setup()
      renderWithProviders(
        <InstanceActions instanceId="i-123" state="running" />,
      )
      await user.click(screen.getByText("Terminate"))
      expect(screen.getByText(TERMINATE_CONFIRM_RE)).toBeInTheDocument()
      expect(screen.getByText(/i-123/)).toBeInTheDocument()
    })

    it("calls terminate mutation only after confirming", async () => {
      const user = userEvent.setup()
      renderWithProviders(
        <InstanceActions instanceId="i-123" state="running" />,
      )
      await user.click(screen.getByText("Terminate"))
      const dialog = screen.getByRole("alertdialog")
      await user.click(
        within(dialog).getByRole("button", { name: "Terminate" }),
      )
      expect(mockMutate).toHaveBeenCalledWith(
        "i-123",
        expect.objectContaining({ onSettled: expect.any(Function) }),
      )
    })

    it("closes dialog without calling mutation when Cancel clicked", async () => {
      const user = userEvent.setup()
      renderWithProviders(
        <InstanceActions instanceId="i-123" state="running" />,
      )
      await user.click(screen.getByText("Terminate"))
      await user.click(screen.getByText("Cancel"))
      expect(mockMutate).not.toHaveBeenCalled()
      expect(screen.queryByText(TERMINATE_CONFIRM_RE)).not.toBeInTheDocument()
    })
  })
})
