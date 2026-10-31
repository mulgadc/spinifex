import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { FormLock } from "./form-lock"
import { Input } from "./ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger } from "./ui/select"
import { Switch } from "./ui/switch"

function Fields({
  locked,
  onSwitch,
}: {
  locked: boolean
  onSwitch: () => void
}) {
  return (
    <FormLock locked={locked}>
      <Input aria-label="Name" />
      <Switch aria-label="Public" onCheckedChange={onSwitch} />
      <Select>
        <SelectTrigger aria-label="Tenancy" />
        <SelectContent>
          <SelectItem value="default">default</SelectItem>
        </SelectContent>
      </Select>
    </FormLock>
  )
}

describe("FormLock", () => {
  it("disables inputs, switches and selects while locked", async () => {
    const onSwitch = vi.fn()

    render(<Fields locked onSwitch={onSwitch} />)

    expect(screen.getByRole("textbox", { name: "Name" })).toBeDisabled()
    expect(screen.getByRole("combobox", { name: "Tenancy" })).toBeDisabled()

    await userEvent.click(screen.getByRole("switch", { name: "Public" }))

    expect(onSwitch).not.toHaveBeenCalled()
  })

  it("releases the controls once unlocked", async () => {
    const onSwitch = vi.fn()

    render(<Fields locked={false} onSwitch={onSwitch} />)

    expect(screen.getByRole("textbox", { name: "Name" })).toBeEnabled()

    await userEvent.click(screen.getByRole("switch", { name: "Public" }))

    expect(onSwitch).toHaveBeenCalledOnce()
  })
})
