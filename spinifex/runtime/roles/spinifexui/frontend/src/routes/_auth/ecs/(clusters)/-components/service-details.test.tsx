import type { Service, Task } from "@aws-sdk/client-ecs"
import { screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(vi.fn())
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import type { PanelTab } from "@/lib/panel-search"

import { ServiceDetails } from "./service-details"

const TASK_ID = "task0001"
const TASK_ARN = `arn:aws:ecs:ap-southeast-2:123456789012:task/web/${TASK_ID}`

const SERVICE: Service = {
  serviceName: "web-svc",
  serviceArn: "arn:aws:ecs:ap-southeast-2:123456789012:service/web/web-svc",
  status: "ACTIVE",
  desiredCount: 1,
  runningCount: 1,
  pendingCount: 0,
  taskDefinition:
    "arn:aws:ecs:ap-southeast-2:123456789012:task-definition/app:3",
  launchType: "EC2",
  loadBalancers: [
    {
      targetGroupArn:
        "arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:targetgroup/tg/abc",
      containerName: "app",
      containerPort: 80,
    },
  ],
  events: [{ id: "e1", message: "service reached steady state" }],
}

const TASKS: Task[] = [
  {
    taskArn: TASK_ARN,
    lastStatus: "RUNNING",
    group: "service:web-svc",
    attachments: [
      {
        type: "ElasticNetworkInterface",
        details: [
          { name: "privateIPv4Address", value: "10.0.1.5" },
          { name: "publicIPv4Address", value: "54.1.2.3" },
        ],
      },
    ],
  },
]

function renderDetails(tab: PanelTab = "overview") {
  renderWithClient(
    <ServiceDetails
      clusterName="web"
      layout="page"
      onClose={vi.fn()}
      onTabChange={vi.fn()}
      service={SERVICE}
      tab={tab}
      tasks={TASKS}
    />,
    createTestQueryClient(),
  )
}

describe("ServiceDetails", () => {
  it("lists member tasks linking to the task page", () => {
    renderDetails()

    expect(screen.getByRole("link", { name: TASK_ID })).toHaveAttribute(
      "href",
      `/ecs/list-clusters/web/tasks/${TASK_ID}`,
    )
    expect(screen.getByText(/10\.0\.1\.5/u)).toBeInTheDocument()
    expect(screen.getByText(/54\.1\.2\.3/u)).toBeInTheDocument()
  })

  it("renders load balancers and config on Overview", () => {
    renderDetails()

    expect(screen.getByText("Container app, port 80")).toBeInTheDocument()
    expect(screen.getByText("app:3")).toBeInTheDocument()
  })

  it("renders events on Settings", () => {
    renderDetails("settings")

    expect(screen.getByText("service reached steady state")).toBeInTheDocument()
  })
})
