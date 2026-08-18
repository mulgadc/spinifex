import { zodResolver } from "@hookform/resolvers/zod"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { useForm } from "react-hook-form"
import { describe, expect, it, vi } from "vitest"
import { z } from "zod"

import { rdsTagSchema } from "@/types/rds"

import { TagsFieldArray } from "./tags-field-array"

const schema = z.object({ tags: z.array(rdsTagSchema) })
type FormData = z.infer<typeof schema>

function TagsForm({ onSubmit }: { onSubmit: (data: FormData) => void }) {
  const { control, handleSubmit } = useForm<FormData>({
    resolver: zodResolver(schema),
    defaultValues: { tags: [] },
  })

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <TagsFieldArray control={control} name="tags" />
      <button type="submit">Submit</button>
    </form>
  )
}

describe("TagsFieldArray", () => {
  it("shows the nested validation error that blocks submission", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(<TagsForm onSubmit={onSubmit} />)

    await user.click(screen.getByRole("button", { name: "Add tag" }))
    await user.click(screen.getByRole("button", { name: "Submit" }))

    expect(screen.getByText("Tag key is required")).toBeInTheDocument()
    expect(screen.getByPlaceholderText("Key")).toHaveAttribute(
      "aria-invalid",
      "true",
    )
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it("submits valid tag rows", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(<TagsForm onSubmit={onSubmit} />)

    await user.click(screen.getByRole("button", { name: "Add tag" }))
    await user.type(screen.getByPlaceholderText("Key"), "environment")
    await user.type(screen.getByPlaceholderText("Value"), "production")
    await user.click(screen.getByRole("button", { name: "Submit" }))

    expect(onSubmit).toHaveBeenCalledWith(
      { tags: [{ key: "environment", value: "production" }] },
      expect.anything(),
    )
  })
})
