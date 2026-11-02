import { zodResolver } from "@hookform/resolvers/zod"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { useForm } from "react-hook-form"
import { describe, expect, it, vi } from "vitest"
import { z } from "zod"

import { SelectField, selectOptions, TextField } from "./form-fields"
import type { PickerNotice } from "./picker-notice"

const schema = z.object({
  size: z.string().min(1, "Pick a size"),
  name: z.string().min(1, "Name is required"),
})

type FormData = z.infer<typeof schema>

function TestForm({
  onSubmit,
  onSizeChange,
  notice,
}: {
  onSubmit: (data: FormData) => void
  onSizeChange?: (value: string) => void
  notice?: PickerNotice
}) {
  const { control, handleSubmit, register, formState } = useForm<FormData>({
    resolver: zodResolver(schema),
    defaultValues: { size: "", name: "" },
  })

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <SelectField
        control={control}
        id="size"
        label="Size"
        name="size"
        notice={notice}
        onValueChange={onSizeChange}
        options={selectOptions(["small", "large"])}
        placeholder="Select a size"
      />
      <TextField
        description="Shown on the console."
        error={formState.errors.name}
        id="name"
        label="Name"
        {...register("name")}
      />
      <button type="submit">Submit</button>
    </form>
  )
}

function RoleForm() {
  const { control } = useForm({
    defaultValues: { role: "arn:aws:iam::000000000001:role/node" },
  })

  return (
    <SelectField
      control={control}
      id="role"
      label="Node role"
      name="role"
      options={[
        { value: "arn:aws:iam::000000000001:role/node", label: "node" },
      ]}
    />
  )
}

describe("SelectField", () => {
  it("shows the chosen option's label, not its value", () => {
    render(<RoleForm />)

    const trigger = screen.getByRole("combobox", { name: "Node role" })

    expect(trigger).toHaveTextContent("node")
    expect(trigger).not.toHaveTextContent("arn:")
  })

  it("writes the chosen option into the form", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(<TestForm onSubmit={onSubmit} />)

    await user.click(screen.getByLabelText("Size"))
    await user.click(await screen.findByRole("option", { name: "large" }))
    await user.type(screen.getByLabelText("Name"), "web")
    await user.click(screen.getByRole("button", { name: "Submit" }))

    expect(onSubmit).toHaveBeenCalledWith(
      { size: "large", name: "web" },
      expect.anything(),
    )
  })

  it("hands the choice to onValueChange instead of the form when given", async () => {
    const user = userEvent.setup()
    const onSizeChange = vi.fn()
    render(<TestForm onSizeChange={onSizeChange} onSubmit={vi.fn()} />)

    await user.click(screen.getByLabelText("Size"))
    await user.click(await screen.findByRole("option", { name: "small" }))

    expect(onSizeChange).toHaveBeenCalledWith("small")
    expect(screen.getByLabelText("Size")).toHaveTextContent("Select a size")
  })

  it("shows the validation error against the control", async () => {
    const user = userEvent.setup()
    render(<TestForm onSubmit={vi.fn()} />)

    await user.click(screen.getByRole("button", { name: "Submit" }))

    await expect(screen.findByText("Pick a size")).resolves.toBeInTheDocument()
    expect(screen.getByLabelText("Size")).toHaveAttribute(
      "aria-invalid",
      "true",
    )
  })

  it("replaces the control with the notice", () => {
    render(
      <TestForm
        notice={{ tone: "error", text: "Could not read the sizes" }}
        onSubmit={vi.fn()}
      />,
    )

    expect(screen.getByText("Could not read the sizes")).toBeInTheDocument()
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument()
  })
})

describe("TextField", () => {
  it("shows the validation error and marks the input invalid", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(<TestForm onSubmit={onSubmit} />)

    await user.click(screen.getByRole("button", { name: "Submit" }))

    await expect(
      screen.findByText("Name is required"),
    ).resolves.toBeInTheDocument()
    expect(screen.getByLabelText("Name")).toHaveAttribute(
      "aria-invalid",
      "true",
    )
    expect(screen.getByText("Shown on the console.")).toBeInTheDocument()
    expect(onSubmit).not.toHaveBeenCalled()
  })
})
