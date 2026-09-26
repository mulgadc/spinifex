import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { SSHAccessCard } from "./ssh-access-card"

const oracleImage = { Name: "ami-oracle-10.1-x86_64" }

describe("SSHAccessCard", () => {
  it("shows the command with the AMI's own login user", () => {
    render(
      <SSHAccessCard
        image={oracleImage}
        instance={{
          PublicIpAddress: "149.118.74.90",
          KeyName: "oracle-test",
        }}
      />,
    )
    expect(
      screen.getByText(
        "ssh -i path/to/oracle-test.pem cloud-user@149.118.74.90",
      ),
    ).toBeInTheDocument()
  })

  it("warns that a private-only instance needs a host inside the VPC", () => {
    render(
      <SSHAccessCard
        image={oracleImage}
        instance={{ PrivateIpAddress: "172.31.0.4", KeyName: "oracle-test" }}
      />,
    )
    expect(
      screen.getByText(/reachable only from inside the VPC/),
    ).toBeInTheDocument()
  })

  it("says so plainly when the AMI does not identify a login user", () => {
    render(
      <SSHAccessCard
        image={{ Name: "my-appliance" }}
        instance={{ PublicIpAddress: "149.118.74.90" }}
      />,
    )
    expect(
      screen.getByText(/could not be determined from this AMI/),
    ).toBeInTheDocument()
    expect(
      screen.getByText("ssh -i path/to/key.pem <user>@149.118.74.90"),
    ).toBeInTheDocument()
  })

  it("renders nothing for an instance with no address", () => {
    const { container } = render(
      <SSHAccessCard image={oracleImage} instance={{}} />,
    )
    expect(container).toBeEmptyDOMElement()
  })
})
