import type { CertificateSummary } from "@aws-sdk/client-acm"
import type {
  SslPolicy,
  TargetGroup,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { useForm } from "react-hook-form"
import { describe, expect, it } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"
import type { CreateListenerFormData } from "@/types/elbv2"

import { ListenerForm } from "./listener-form"

const TGS: TargetGroup[] = [
  {
    TargetGroupArn: "arn:tg/a",
    TargetGroupName: "tg-a",
    Protocol: "HTTP",
    Port: 80,
    VpcId: "vpc-aaa",
  },
  {
    TargetGroupArn: "arn:tg/b",
    TargetGroupName: "tg-b",
    Protocol: "HTTP",
    Port: 8080,
    VpcId: "vpc-aaa",
  },
]

const CERTS: CertificateSummary[] = [
  {
    CertificateArn: "arn:aws:acm:ap-southeast-2:1:certificate/abc-123",
    DomainName: "example.com",
  },
]

const POLICIES: SslPolicy[] = [
  { Name: "ELBSecurityPolicy-2016-08" },
  { Name: "ELBSecurityPolicy-TLS13-1-2-2021-06" },
]

function Harness({
  targetGroups,
  protocol = "HTTP",
}: {
  targetGroups: TargetGroup[]
  protocol?: "HTTP" | "HTTPS"
}) {
  const form = useForm<CreateListenerFormData>({
    defaultValues: {
      protocol,
      port: protocol === "HTTPS" ? 443 : 80,
      defaultTargetGroupArn: "",
    },
  })
  return (
    <ListenerForm
      certificates={CERTS}
      form={form}
      sslPolicies={POLICIES}
      targetGroups={targetGroups}
    />
  )
}

function renderHarness(props: Parameters<typeof Harness>[0]) {
  return renderWithClient(<Harness {...props} />, createTestQueryClient())
}

describe("ListenerForm", () => {
  it("renders protocol, port, and target-group selectors", () => {
    renderHarness({ targetGroups: TGS })
    expect(screen.getByLabelText("Protocol")).toBeInTheDocument()
    expect(screen.getByLabelText("Port")).toBeInTheDocument()
    expect(screen.getByLabelText("Default target group")).toBeInTheDocument()
  })

  it("shows empty-state message when no target groups available", () => {
    renderHarness({ targetGroups: [] })
    expect(
      screen.getByText(/no target groups available in this vpc/i),
    ).toBeInTheDocument()
  })

  it("hides certificate and security-policy fields for HTTP", () => {
    renderHarness({ targetGroups: TGS, protocol: "HTTP" })
    expect(screen.queryByLabelText("Certificate")).not.toBeInTheDocument()
    expect(screen.queryByLabelText("Security policy")).not.toBeInTheDocument()
  })

  it("reveals certificate and security-policy fields for HTTPS", () => {
    renderHarness({ targetGroups: TGS, protocol: "HTTPS" })
    expect(screen.getByLabelText("Certificate")).toBeInTheDocument()
    expect(screen.getByLabelText("Security policy")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: /import certificate/i }),
    ).toBeInTheDocument()
  })

  // Guards the React Compiler regression: the TLS fields are gated on a
  // watched value. A plain `watch("protocol")` read gets memoised once by the
  // compiler and never updates, so switching protocol at runtime never reveals
  // the fields. `useWatch` keeps the subscription reactive — this test fails if
  // it regresses to `watch`.
  it("reveals certificate fields when protocol switches HTTP -> HTTPS at runtime", async () => {
    function ToggleHarness() {
      const form = useForm<CreateListenerFormData>({
        defaultValues: {
          protocol: "HTTP",
          port: 80,
          defaultTargetGroupArn: "",
        },
      })
      return (
        <>
          <button
            onClick={() => form.setValue("protocol", "HTTPS")}
            type="button"
          >
            go-https
          </button>
          <ListenerForm
            certificates={CERTS}
            form={form}
            sslPolicies={POLICIES}
            targetGroups={TGS}
          />
        </>
      )
    }

    renderWithClient(<ToggleHarness />, createTestQueryClient())
    expect(screen.queryByLabelText("Certificate")).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole("button", { name: "go-https" }))

    await expect(
      screen.findByLabelText("Certificate"),
    ).resolves.toBeInTheDocument()
    expect(screen.getByLabelText("Security policy")).toBeInTheDocument()
  })
})
