import { fireEvent, screen, waitFor, within } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
}))

import { ParametersEditor } from "./parameters-editor"

const DYNAMIC_PARAM = {
  ParameterName: "log_min_duration_statement",
  ParameterValue: "-1",
  Description: "Log statements slower than this",
  Source: "engine-default",
  ApplyType: "dynamic",
  ApplyMethod: "immediate",
  DataType: "integer",
  AllowedValues: "-1-2147483647 (ms)",
  IsModifiable: true,
}

const STATIC_PARAM = {
  ParameterName: "max_connections",
  ParameterValue: "100",
  Description: "Maximum concurrent connections",
  Source: "user",
  ApplyType: "static",
  ApplyMethod: "pending-reboot",
  DataType: "integer",
  AllowedValues: "6-8388607",
  IsModifiable: true,
}

const FIXED_PARAM = {
  ParameterName: "ssl",
  ParameterValue: "1",
  Description: "Serve TLS",
  Source: "engine-default",
  ApplyType: "static",
  ApplyMethod: "pending-reboot",
  DataType: "boolean",
  AllowedValues: "0,1",
  IsModifiable: false,
}

function render(
  parameters: unknown[] = [DYNAMIC_PARAM, STATIC_PARAM, FIXED_PARAM],
  readOnly = false,
) {
  return renderWithClient(
    <ParametersEditor
      dbParameterGroupName="orders-pg"
      parameters={parameters as never}
      readOnly={readOnly}
    />,
    createTestQueryClient(),
  )
}

function valueInput(name: string): HTMLElement {
  return screen.getByLabelText(`Value of ${name}`)
}

describe("ParametersEditor", () => {
  it("renders every parameter with its source and allowed values", () => {
    render()
    expect(screen.getByText("max_connections")).toBeInTheDocument()
    expect(screen.getByText("6-8388607")).toBeInTheDocument()
    expect(screen.getByText("user")).toBeInTheDocument()
    expect(screen.getAllByText("engine-default")).not.toHaveLength(0)
  })

  // The backend refuses an unmodifiable parameter by design; hiding it would
  // read as the engine not offering the setting at all.
  it("shows an unmodifiable parameter without a control", () => {
    render()
    expect(screen.getByText(/fixed by the platform/)).toBeInTheDocument()
    expect(screen.queryByLabelText("Value of ssl")).not.toBeInTheDocument()
  })

  it("filters by parameter name", () => {
    render()
    fireEvent.change(screen.getByLabelText("Filter parameters"), {
      target: { value: "max_conn" },
    })
    expect(screen.getByText("max_connections")).toBeInTheDocument()
    expect(screen.queryByText("ssl")).not.toBeInTheDocument()
  })

  it("reports a filter that matches nothing", () => {
    render()
    fireEvent.change(screen.getByLabelText("Filter parameters"), {
      target: { value: "nothing_matches_this" },
    })
    expect(
      screen.getByText("No parameter matches this filter."),
    ).toBeInTheDocument()
  })

  // resolveApplyMethod rejects "immediate" on a static parameter rather than
  // downgrading it, so the choice must never be offered.
  it("pins a static parameter to pending-reboot", () => {
    render()
    fireEvent.change(valueInput("max_connections"), {
      target: { value: "200" },
    })
    expect(
      screen.queryByLabelText("Apply method of max_connections"),
    ).not.toBeInTheDocument()

    const row = screen.getByText("max_connections").closest("tr")
    expect(
      within(row as HTMLElement).getByText(/adopted at the next reboot/),
    ).toBeInTheDocument()
  })

  it("offers the apply method once a dynamic parameter is edited", () => {
    render()
    expect(
      screen.queryByLabelText("Apply method of log_min_duration_statement"),
    ).not.toBeInTheDocument()

    fireEvent.change(valueInput("log_min_duration_statement"), {
      target: { value: "500" },
    })

    expect(
      screen.getByLabelText("Apply method of log_min_duration_statement"),
    ).toBeInTheDocument()
  })

  it("sends the edited parameters with their apply methods", async () => {
    render()
    fireEvent.change(valueInput("max_connections"), {
      target: { value: "200" },
    })
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }))

    await waitFor(() => expect(mockSend).toHaveBeenCalled())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBParameterGroupName).toBe("orders-pg")
    expect(input.Parameters).toStrictEqual([
      {
        ParameterName: "max_connections",
        ParameterValue: "200",
        ApplyMethod: "pending-reboot",
      },
    ])
  })

  it("discards an edit without sending anything", () => {
    render()
    fireEvent.change(valueInput("max_connections"), {
      target: { value: "200" },
    })
    fireEvent.click(screen.getByRole("button", { name: "Discard" }))
    expect(screen.queryByRole("button", { name: "Save changes" })).toBeNull()
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("refuses to save a blank value", () => {
    render()
    fireEvent.change(valueInput("max_connections"), { target: { value: "" } })
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled()
    expect(screen.getByText(/cannot be left blank/)).toBeInTheDocument()
  })

  // ModifyDBParameterGroup takes at most 20 per call, and each call is atomic,
  // so the cap is enforced here rather than by a partial multi-request save.
  it("blocks a save of more than twenty parameters", () => {
    const many = Array.from({ length: 21 }, (_, i) => ({
      ...DYNAMIC_PARAM,
      ParameterName: `param_${i}`,
    }))
    render(many)

    for (let i = 0; i < 21; i++) {
      fireEvent.change(valueInput(`param_${i}`), { target: { value: "5" } })
    }

    expect(screen.getByText("21 parameters edited")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled()
    expect(
      screen.getByText(/At most 20 parameters can be saved at once/),
    ).toBeInTheDocument()
  })

  it("surfaces a rejected modify", async () => {
    mockSend.mockRejectedValueOnce(
      new Error("parameter max_connections is static"),
    )
    render()
    fireEvent.change(valueInput("max_connections"), {
      target: { value: "200" },
    })
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }))

    expect(
      await screen.findByText(/parameter max_connections is static/),
    ).toBeInTheDocument()
  })

  it("offers no control at all on a read-only group", () => {
    render(undefined, true)
    expect(screen.queryByLabelText("Value of max_connections")).toBeNull()
    expect(
      within(screen.getByRole("table")).getByText("100"),
    ).toBeInTheDocument()
  })
})
