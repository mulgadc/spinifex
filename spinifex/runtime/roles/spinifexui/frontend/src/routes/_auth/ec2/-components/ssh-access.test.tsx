import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { PanelFields } from "@/components/detail-panel"

import { SSHAccess } from "./ssh-access"

const oracleImage = { Name: "ami-oracle-10.1-x86_64" }

function renderAccess(props: Parameters<typeof SSHAccess>[0]) {
  return render(
    <PanelFields>
      <SSHAccess {...props} />
    </PanelFields>,
  )
}

describe("SSHAccess", () => {
  it("shows the command with the AMI's own login user", () => {
    renderAccess({
      image: oracleImage,
      instance: { PublicIpAddress: "149.118.74.90", KeyName: "oracle-test" },
    })

    expect(
      screen.getByText(
        "ssh -i path/to/oracle-test.pem cloud-user@149.118.74.90",
      ),
    ).toBeInTheDocument()
    expect(screen.getByText("cloud-user")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Copy SSH" })).toBeInTheDocument()
  })

  it("warns that a private-only instance needs a host inside the VPC", () => {
    renderAccess({
      image: oracleImage,
      instance: { PrivateIpAddress: "172.31.0.4", KeyName: "oracle-test" },
    })

    expect(
      screen.getByText(/reachable only from inside the VPC/u),
    ).toBeInTheDocument()
  })

  it("says so plainly when the AMI does not identify a login user", () => {
    renderAccess({
      image: { Name: "my-appliance" },
      instance: { PublicIpAddress: "149.118.74.90" },
    })

    expect(
      screen.getByText(/could not be determined from this AMI/u),
    ).toBeInTheDocument()
    expect(
      screen.getByText("ssh -i path/to/key.pem <user>@149.118.74.90"),
    ).toBeInTheDocument()
  })

  it("adds nothing for an instance with no address", () => {
    const { container } = renderAccess({ image: oracleImage, instance: {} })

    expect(container.querySelector("dl")).toBeEmptyDOMElement()
  })
})
