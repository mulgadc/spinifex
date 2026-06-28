# ECS Quickstart

An end-to-end ECS stack on Spinifex, provisioned with OpenTofu: a cluster, a
task definition with a task role, container instances launched from the
`spinifex-ecs-node` AMI, and an awsvpc service fronted by an Application Load
Balancer target group.

This is the Terraform-native equivalent of the console's "provision capacity"
action. Because a Spinifex container instance reaches the control plane over the
gateway (TLS + SigV4) rather than a managed AWS endpoint, the workbook makes the
two things the console injects for you explicit: a LAN-reachable gateway URL and
the gateway CA, both baked into the instance's cloud-init user-data. The agent
draws its credentials from IMDS via the `ecsInstanceRole` instance profile, so
no static keys are written.

## Prerequisites

- The `spinifex-ecs-node` AMI is imported (resolved here by the
  `tag:spinifex:managed-by=ecs` filter):

  ```
  aws ec2 describe-images \
    --filters 'Name=tag:spinifex:managed-by,Values=ecs' \
    --query 'Images[].[ImageId,Name]' --output text
  ```

- A **LAN-reachable** gateway URL — the host's WAN/bridge IP, not `127.0.0.1`
  (a guest VM cannot reach the host loopback). For example
  `https://192.168.1.33:9999`.

- The gateway CA PEM is readable at `gateway_ca_cert_path` (default
  `/etc/spinifex/ca.pem`).

## Usage

```bash
cd spinifex/docs/terraform-workbooks/ecs-quickstart
export AWS_PROFILE=spinifex

tofu init
tofu apply -var 'gateway_url=https://<host-lan-ip>:9999'
```

`ecsInstanceRole` is account-global. If you have already used the console's
provision-capacity action it exists already, so skip re-creating it:

```bash
tofu apply \
  -var 'gateway_url=https://<host-lan-ip>:9999' \
  -var 'create_instance_role=false'
```

## Verify

Container instances take ~30-60s to boot and register:

```bash
aws ecs list-container-instances --cluster ecs-quickstart
aws ecs describe-services --cluster ecs-quickstart --services ecs-quickstart-web \
  --query 'services[0].[runningCount,desiredCount]'
```

The ALB DNS name ends in `.elb.spinifex.local` and does not resolve from your
host. Fetch its public IP and curl it:

```bash
aws elbv2 describe-load-balancers --names ecs-quickstart-alb \
  --query 'LoadBalancers[0].AvailabilityZones[].LoadBalancerAddresses[].IpAddress' \
  --output text
curl http://<alb_public_ip>
```

## Variables

| Variable | Default | Purpose |
|---|---|---|
| `region` | `ap-southeast-2` | AWS region. |
| `cluster_name` | `ecs-quickstart` | Cluster + resource name prefix. |
| `instance_type` | `t3.small` | Container instance type. |
| `container_count` | `1` | Container instances + service desired count. |
| `task_image` | `docker.io/library/nginx:1.27-alpine` | Image the task runs. |
| `spinifex_endpoint` | `https://127.0.0.1:9999` | Gateway as seen from the host running Terraform. |
| `gateway_url` | _(required)_ | Gateway as seen from a guest VM (LAN-reachable). |
| `gateway_ca_cert_path` | `/etc/spinifex/ca.pem` | Host-readable gateway CA PEM. |
| `create_instance_role` | `true` | Create `ecsInstanceRole`; set `false` if it already exists. |

## Teardown

```bash
tofu destroy -var 'gateway_url=https://<host-lan-ip>:9999'
```

`DeleteCluster` cascades through the service, its tasks, and the container
instance registrations, so the destroy round-trips cleanly.
```
