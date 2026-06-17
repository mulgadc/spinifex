import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { useState } from "react"
import { describe, expect, it, vi } from "vitest"

import { JsonEditor } from "./json-editor"

function Harness({ initial }: { initial: string }) {
  const [value, setValue] = useState(initial)
  return (
    <>
      <JsonEditor aria-label="json" onChange={setValue} value={value} />
      <button type="button">elsewhere</button>
    </>
  )
}

const blurAway = async (user: ReturnType<typeof userEvent.setup>) =>
  await user.click(screen.getByRole("button", { name: "elsewhere" }))

describe("JsonEditor", () => {
  it("pretty-prints valid JSON on blur", async () => {
    const user = userEvent.setup()
    render(<Harness initial='{"a":1}' />)
    const editor = screen.getByLabelText("json")

    await user.click(editor)
    await blurAway(user)

    expect(editor).toHaveValue('{\n  "a": 1\n}')
  })

  it("leaves malformed JSON untouched on blur", async () => {
    const user = userEvent.setup()
    render(<Harness initial="{ not json" />)
    const editor = screen.getByLabelText("json")

    await user.click(editor)
    await blurAway(user)

    expect(editor).toHaveValue("{ not json")
  })

  it("propagates raw input through onChange", async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<JsonEditor aria-label="json" onChange={onChange} value="" />)

    await user.type(screen.getByLabelText("json"), "x")

    expect(onChange).toHaveBeenCalledWith("x")
  })

  it("marks the field invalid when error is set", () => {
    render(<JsonEditor aria-label="json" error onChange={vi.fn()} value="" />)

    expect(screen.getByLabelText("json")).toHaveAttribute(
      "aria-invalid",
      "true",
    )
  })
})
