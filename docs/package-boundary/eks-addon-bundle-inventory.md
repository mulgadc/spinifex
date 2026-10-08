# EKS managed add-on: per-bundle readiness and removal inventory

Code read at 3a55e2ee1; not verified live.

This is a discovery record for the add-on lifecycle work: for every bundle in the catalogue it lists what is rendered, how identity is wired, what "ready" currently proves, what removal deletes and leaves, and where the bundle collides with other objects.
It proposes no design; gaps are stated per bundle and collected at the end.
It complements `docs/package-boundary/eks-addon-lifecycle.md` (present contract) and `docs/package-boundary/eks-addon-lifecycle-design.md` (draft correction, which refines open proposal Q-146: delete holds until owned objects and identity associations are observably removed, and `ACTIVE` means workloads applied and ready).
Paths starting `spinifex/` are Go sources; `scripts/` and `tests/` are relative to the spinifex repo root; bundle files are under `scripts/images/eks-node/addons/<addon>/<version>/` and are abbreviated to `<addon>/<version>/<file>`.
The sync agent `scripts/images/eks-node/mulga-eks-addon-sync.sh` is abbreviated to `sync.sh`.

## 1. Catalogue

| Name | Version | `RequiresIRSA` | Hidden | Evidence |
|---|---|---|---|---|
| `aws-load-balancer-controller` | 2.11.0 | true | no | `spinifex/domains/eks/addon/catalog.go:31-33` |
| `argocd` | 3.0.23 | false | no | `catalog.go:34-36` |
| `aws-ebs-csi-driver` | 1.40.1 | true | no | `catalog.go:37-39` |
| `nvidia-device-plugin` | 0.17.4 | false | yes (auto-staged by GPU node groups) | `catalog.go:42-44`, `spinifex/domains/eks/addon/owner.go:79-89`, `spinifex/handlers/eks/nodegroup.go:408-416` |
| `spinifex-noop` | 0.1.0 | false | yes (e2e fixture) | `catalog.go:48-50` |

`RequiresIRSA` is read only by `DescribeAddonVersions` as `requiresIamPermissions` (`spinifex/handlers/eks/addons.go:255`); `CreateAddon` neither requires a role for the two `true` bundles nor rejects one for the three `false` bundles (`addons.go:61-99`).
The bundle manifests are baked into the AMI (`scripts/images/eks-node/manifest.conf:58-67`), but container images are not: every image below is pulled from a public registry at pod start.

## 2. AWS reference semantics

Checked against the API reference on 2026-10-09.

| Item | AWS documents | Source |
|---|---|---|
| `status` | Enum `CREATING`, `ACTIVE`, `CREATE_FAILED`, `UPDATING`, `DELETING`, `DELETE_FAILED`, `DEGRADED`, `UPDATE_FAILED`; the API reference gives no per-value definition of `ACTIVE` or `DEGRADED`. | API_Addon |
| `health` | `AddonHealth.issues`, a list of `AddonIssue`. | API_AddonHealth |
| `AddonIssue` | `code` one of `AccessDenied`, `InternalFailure`, `ClusterUnreachable`, `InsufficientNumberOfReplicas`, `ConfigurationConflict`, `AdmissionRequestDenied`, `UnsupportedAddonModification`, `K8sResourceNotFound`, `AddonSubscriptionNeeded`, `AddonPermissionFailure`; plus `message` and `resourceIds`. | API_AddonIssue |
| `serviceAccountRoleArn` | "The ARN of the IAM role that's bound to the Kubernetes `ServiceAccount` object that the add-on uses"; on create, "If you don't specify an existing IAM role, then the add-on uses the permissions assigned to the node IAM role", and an IAM OIDC provider must exist for the cluster. | API_Addon, API_CreateAddon |
| `configurationValues` | "validated against the schema returned by `DescribeAddonConfiguration`". | API_CreateAddon |
| `resolveConflicts` | `NONE`/`PRESERVE` leave a self-managed install's values and may fail creation; `OVERWRITE` replaces them; with no prior install all three behave alike. | API_CreateAddon |
| `DeleteAddon` | "When you remove an add-on, it's deleted from the cluster"; `preserve` "preserves the add-on software on your cluster but Amazon EKS stops managing any settings for the add-on. If an IAM account is associated with the add-on, it isn't removed." The example response is `DELETING`. | API_DeleteAddon |

The issue codes imply AWS distinguishes replica shortfall (`InsufficientNumberOfReplicas`), missing objects (`K8sResourceNotFound`), field conflicts (`ConfigurationConflict`) and admission denials; how EKS computes them, and whether `DEGRADED` is driven by them, is not stated in the API reference and was not verified.
Whether AWS offers each of these five names as an EKS add-on was not checked against a live `DescribeAddonVersions`.

## 3. Shared mechanics (apply to every bundle)

### 3.1 Render

| Step | Behaviour | Evidence |
|---|---|---|
| Input | One TSV line per staged add-on: `name`, `version`, `roleArn`, `base64(configurationValues)`. | `spinifex/agents/eks/gatewayfetch/fetch.go` `emitAddonsTSV`, `sync.sh:357` |
| Source | Every `*.yaml` in `/usr/share/spinifex-eks/addons/<name>/<version>/`, in glob order, concatenated with `---`. A missing directory reports `failed`. | `sync.sh:41`, `sync.sh:82-89`, `sync.sh:219-242` |
| Output | One file per add-on name, `/var/lib/rancher/k3s/server/manifests/spinifex-addon-<name>.yaml`; the name carries no version. Replaced only when content changes. | `sync.sh:42-43`, `sync.sh:83`, `sync.sh:248-253` |
| Block markers | `{{IRSA_ENV}}`, `{{IRSA_VOLUME}}`, `{{IRSA_VOLUME_MOUNT}}`, `{{ELB_INGRESS_PARAMS_SPEC}}` replaced by awk when alone on a line. | `sync.sh:224-230` |
| Scalar placeholders | `{{SERVICE_ACCOUNT_ROLE_ARN}}`, `{{CLUSTER_NAME}}`, `{{AWS_REGION}}`, `{{AWS_VPC_ID}}`, `{{GATEWAY_ENDPOINT}}`, `{{GATEWAY_CA_PEM_B64}}`, `{{WEBHOOK_CA_B64}}`, `{{WEBHOOK_TLS_CRT_B64}}`, `{{WEBHOOK_TLS_KEY_B64}}` via `sed s|..|..|g`. The role ARN is inserted raw: it is not escaped for sed (`|`, `&`, `\`) or for YAML. | `sync.sh:231-240` |
| Webhook cert | Opt-in by `webhook.conf`; minted once per add-on under `/var/lib/spinifex-eks/webhook-certs/<name>`, 10-year validity, reused thereafter. | `sync.sh:98-120`, `spinifex/agents/eks/webhookcert/cert.go:109-110` |
| `configurationValues` | Read from the TSV and discarded; no bundle has a hook. | `sync.sh:359` |
| Executor | Primary server only; server-join nodes do not run the agent. | `scripts/images/eks-node/eks-node-role.sh:138-149` |

### 3.2 Identity (IRSA) wiring

| Element | Present behaviour | Evidence |
|---|---|---|
| Role carrier | The role ARN lives in the add-on record and the staged manifest (`serviceAccountRoleArn`), and after render in the rendered file only. No pod identity association, no EKS-side binding object. | `spinifex/domains/eks/addon/record.go:24-45`, `sync.sh:144-160` |
| Credential env | With a role: `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_STS_REGIONAL_ENDPOINTS`, region, `AWS_ENDPOINT_URL_STS` (gateway), `AWS_CA_BUNDLE` and `SSL_CERT_FILE`. Without one: region and CA only, so the SDK falls through to node instance-profile credentials over IMDS. | `sync.sh:144-195` |
| Token | A projected ServiceAccount token, audience `sts.amazonaws.com`, 86400 s, mounted at `/var/run/secrets/eks.amazonaws.com/serviceaccount`, injected statically by the render. There is no pod-identity mutating webhook in the image, so the `eks.amazonaws.com/role-arn` ServiceAccount annotation is descriptive only; the env and volume are what take effect. | `sync.sh:161-178`; no `pod-identity-webhook` in the tree |
| Issuer | The apiserver's `/openid/v1/jwks` is published at first boot; STS `AssumeRoleWithWebIdentity` resolves `iss` to a registered IAM OIDC provider in the role's account, fetches the cluster JWKS, and evaluates the role trust policy (`{iss}:sub`, `{iss}:aud`). | `scripts/images/eks-node/k3s-first-boot.sh:141-162`, `spinifex/handlers/sts/assume_role_with_web_identity.go:33-213`, `spinifex/handlers/sts/web_identity_trust_policy.go:137-170` |
| Objects outside the bundle | The IAM role, its trust policy (naming `system:serviceaccount:<ns>:<sa>`) and the IAM OIDC provider are user-owned IAM objects; nothing in the add-on path creates, modifies or deletes them. | `tests/e2e/eks/eks_test.go:658-700` creates them in the test |
| Authorisation | `iam:PassRole` on the role at `CreateAddon` and `UpdateAddon`. | `spinifex/gateway/eks/passrole.go:31-37` |
| `eks-token-webhook` | Not part of IRSA: it is the apiserver TokenReview authenticator for `aws eks get-token` bearer tokens. | `scripts/images/eks-node/eks-token-webhook.initd:1-8` |

Because identity association lives only in IAM (user-owned) and in the rendered pod spec, "identity associations observably removed" has no Spinifex-side object to observe for any bundle today: the role is not detached from anything when the add-on is deleted, matching AWS only in that AWS also does not delete the IAM role.

### 3.3 Readiness

| Path | Condition | Evidence |
|---|---|---|
| Primary | The K3s `Addon` CR in `kube-system` whose `.spec.source` is the rendered file carries a non-empty `addon.k3s.cattle.io/gvks` annotation. Reported `ready` on every tick this holds. No pod, replica or CRD condition is consulted. | `sync.sh:266-273`, `sync.sh:361-365` |
| Fallback | Only after `ADDON_READY_GRACE` (90 s) with the annotation still empty: `kubectl get -f <rendered> --ignore-not-found`, then require at least one Deployment, StatefulSet or DaemonSet and that every one returned is rolled out (Deployment `availableReplicas >= spec.replicas` and generation observed; StatefulSet `readyReplicas >= spec.replicas`; DaemonSet `desiredNumberScheduled > 0` and `numberReady >= desiredNumberScheduled`). | `sync.sh:56`, `sync.sh:301-320`, `sync.sh:367-372` |
| Otherwise | `applied`, which changes no status. | `sync.sh:374-376`, `spinifex/domains/eks/addon/owner.go:186-205` |
| `failed` | Emitted only for a missing bundle directory or webhook cert failure; never for apply errors, crash-looping pods or lost replicas. | `sync.sh:87`, `sync.sh:112` |
| Status mapping | `ready` lifts any non-`ACTIVE` status (including `CREATE_FAILED`, `DEGRADED`, `UPDATING`) to `ACTIVE` and clears `health`; `failed` maps `ACTIVE` to `DEGRADED`. Reports carry no generation. | `owner.go:140-168`, `owner.go:186-205` |

Consequences common to all bundles:

1. On the primary path `ready` means "K3s recorded the GVKs it applied", not "workloads running"; a bundle whose pods never start (image pull failure, unschedulable, crash loop) is `ACTIVE` and stays so.
2. `ACTIVE` never degrades on workload loss: the annotation persists, so `ready` is re-sent every tick and no `failed` is ever produced for runtime health.
3. `--ignore-not-found` drops absent objects from the fallback set, so a workload that failed to apply is simply not checked; one rolled-out workload is enough.
4. After `UpdateAddon` the same tick that rewrites the file reads the annotation from the previous apply; whether K3s clears it before that read is not established, so `UPDATING` may settle `ACTIVE` on the old apply (the agent comment at `sync.sh:244-247` says a re-apply clears it, which suggests a window exists).
5. The agent never deletes the K3s `Addon` CR; if K3s keeps that CR after the file is removed, a later re-create would match it by `.spec.source` and could report `ready` from a stale annotation before the new apply (K3s behaviour not verified).

### 3.4 Removal

| Step | Behaviour | Evidence |
|---|---|---|
| Trigger | A rendered `spinifex-addon-<name>.yaml` whose name is absent from a successful fetch. A failed fetch skips GC. | `sync.sh:344-348`, `sync.sh:386-391` |
| Selection | The object list is the rendered file itself (`kubectl delete -f`), not labels or a GVK inventory. Objects created at runtime by the add-on, or by users through it, are never selected. | `sync.sh:397-401` |
| Order | File order, which is glob order of the bundle files then document order. | `sync.sh:219` |
| Waiting | `--wait=false`; errors go to syslog; the copy is deleted; nothing is reported. | `sync.sh:400-402` |
| Leftovers on the node | `/var/lib/spinifex-eks/webhook-certs/<name>` and the K3s `Addon` CR. The `/tmp` sentinels are cleared. | `sync.sh:47`, `sync.sh:393` |
| Control plane | Record and manifest keys are erased before any guest action. | `owner.go:122-137` |
| Namespace completion | Namespace deletion needs every aggregated API available; the e2e asserts `v1beta1.metrics.k8s.io` is `Available` first because otherwise the namespace stays `Terminating`. | `tests/e2e/eks/eks_test.go:197-231` |

`preserve` is ignored (record `eks-addon-lifecycle.md` row Desired state); for every bundle a preserve would need to keep the whole rendered object set and stop only the render and GC, so the per-bundle `preserve` rows below list what in addition would break or dangle.

## 4. `spinifex-noop` 0.1.0

### Objects

| Kind | Name | Namespace | Source |
|---|---|---|---|
| Namespace | `spinifex-noop` | | `spinifex-noop/0.1.0/noop.yaml:5-10` |
| ConfigMap | `spinifex-noop` | `spinifex-noop` | `noop.yaml:12-20` |

Images: none.
Both objects carry `app.kubernetes.io/managed-by: spinifex-addon` (`noop.yaml:9-10`, `noop.yaml:17-18`); nothing reads the label.

### Identity

No IRSA markers, no ServiceAccount; a supplied `serviceAccountRoleArn` is stored and inert.

### Readiness

| Question | Answer |
|---|---|
| Primary path | Ready once K3s records the two GVKs. This is the path the e2e exercises (`tests/e2e/eks/eks_test.go:158-195`). |
| Fallback | Never succeeds: no workload kinds, so `length > 0` is false (`sync.sh:308`). |
| Meaning of ready | Both objects applied; there is nothing to serve. |
| False ready | None material. |

### Removal

| Question | Answer |
|---|---|
| Deleted | Namespace (cascading the ConfigMap) and ConfigMap. |
| Left behind | The K3s `Addon` CR. |
| Preserve | Keep both; nothing dangles. |
| Completion signal | `kubectl get ns spinifex-noop` NotFound; this is exactly what the e2e asserts (`eks_test.go:221-231`). Blocked indefinitely by an unavailable aggregated API. |

### Conflicts and configuration

Namespace name could collide with a user namespace of the same name, which GC would then delete.
No configuration hook.

### Gaps

1. The only bundle with removal proven end-to-end; its completion signal (namespace NotFound) does not generalise to bundles in `kube-system`.

## 5. `argocd` 3.0.23

### Objects

Single file `argocd/3.0.23/argocd.yaml` (26,819 lines), upstream `install.yaml` passed through kustomize with `namespace: argocd` (`argocd.yaml:1-11`).

| Kind | Count | Names | Namespace | Evidence |
|---|---|---|---|---|
| Namespace | 1 | `argocd` (first document) | | `argocd.yaml:12-17` |
| CustomResourceDefinition | 3 | `applications.argoproj.io`, `applicationsets.argoproj.io`, `appprojects.argoproj.io` | | `argocd.yaml:25`, `:5904`, `:23563` |
| ServiceAccount | 7 | `argocd-application-controller`, `-applicationset-controller`, `-dex-server`, `-notifications-controller`, `-redis`, `-repo-server`, `-server` | `argocd` | parsed |
| Role / RoleBinding | 6 / 6 | per component | `argocd` | parsed |
| ClusterRole / ClusterRoleBinding | 3 / 3 | `argocd-application-controller`, `argocd-applicationset-controller`, `argocd-server` | | `argocd.yaml:24243-24554` |
| ConfigMap | 7 | `argocd-cm`, `-cmd-params-cm`, `-gpg-keys-cm`, `-notifications-cm`, `-rbac-cm`, `-ssh-known-hosts-cm`, `-tls-certs-cm` | `argocd` | parsed |
| Secret | 2 | `argocd-notifications-secret`, `argocd-secret` | `argocd` | `argocd.yaml:24740`, `:24751` |
| Service | 8 | applicationset-controller, dex-server, metrics, notifications-controller-metrics, redis, repo-server, server, server-metrics | `argocd` | parsed |
| Deployment | 6 | `argocd-applicationset-controller`, `-dex-server`, `-notifications-controller`, `-redis`, `-repo-server`, `-server` (default 1 replica) | `argocd` | `argocd.yaml:24925-25887` |
| StatefulSet | 1 | `argocd-application-controller`, 1 replica | `argocd` | `argocd.yaml:26301-26310` |
| NetworkPolicy | 7 | one per component | `argocd` | parsed |

Images: `quay.io/argoproj/argocd:v3.0.23` (8 containers), `ghcr.io/dexidp/dex:v2.41.1` (`argocd.yaml:25243`), `redis:7.2.11-alpine` (`argocd.yaml:25478`).
Only the Namespace carries `app.kubernetes.io/managed-by: spinifex-addon`.

### Identity

No IRSA markers (`argocd.yaml:11`); a supplied role is stored and inert.
All seven ServiceAccounts are created by the bundle.

### Readiness

| Question | Answer |
|---|---|
| Primary path | Per the agent comment, K3s never records the `gvks` annotation for this bundle, so the primary path does not fire (`sync.sh:49-55`); why is not established. |
| Fallback | After 90 s: all 6 Deployments available at 1 replica and the StatefulSet at 1 ready replica, among those `kubectl get -f` finds. |
| Meaning of ready | Every returned workload rolled out once. CRDs are not checked for `Established`; an absent CRD or workload is dropped by `--ignore-not-found` rather than failing the check. |
| False ready | A partial apply that left any document unapplied still reads ready if the remaining workloads roll out; if the missing `gvks` is caused by an apply error on one object, this is the case in practice (hypothesis, not verified). Dex and notifications are counted though unused by default. |
| Runtime | Images pulled from three public registries; on an air-gapped site the bundle never rolls out and stays `CREATING`. |

### Removal

| Question | Answer |
|---|---|
| Deleted | Everything in the file, Namespace first. CRD deletion removes every `Application`, `ApplicationSet` and `AppProject` in the cluster, including the runtime `default` AppProject. |
| Left behind | Workloads Argo CD deployed into other namespaces (user applications) unless their `Application` carries the cascade finalizer and the controller survives long enough to process it; runtime Secrets in `argocd` go with the namespace. |
| Stuck risk | `Application` objects carrying `resources-finalizer.argocd.argoproj.io` (user-set, upstream behaviour) need the application controller to clear them; it is deleted in the same pass, so the CRs, the `applications` CRD and the `argocd` namespace can stay `Terminating` indefinitely (not verified live). |
| Preserve | Keep all objects; the cluster-scoped ClusterRoles and CRDs then remain unowned. |
| Completion signal | `argocd` namespace NotFound and the three CRDs NotFound and the three ClusterRoles and ClusterRoleBindings NotFound; namespace and CRD finalizers must be watched explicitly, since NotFound on the namespace alone does not cover cluster-scoped objects. |

### Conflicts and configuration

A user-installed Argo CD (same CRDs, same ClusterRole names, namespace `argocd`) would be overwritten on create and wholly deleted on DeleteAddon; this is the `resolveConflicts` case.
No configuration hook, although `argocd-cm`/`argocd-rbac-cm` are the natural targets.

### Gaps

1. Readiness depends entirely on the fallback, which neither checks CRDs nor detects missing objects.
2. Removal deletes user-owned `Application` data cluster-wide and can wedge on finalizers; nothing reports it.
3. No e2e coverage at all.

## 6. `aws-ebs-csi-driver` 1.40.1

### Objects

| Kind | Name | Namespace | Source |
|---|---|---|---|
| ServiceAccount | `ebs-csi-controller-sa` (role annotation) | `kube-system` | `aws-ebs-csi-driver/1.40.1/00-rbac.yaml:5-14` |
| ServiceAccount | `ebs-csi-node-sa` | `kube-system` | `00-rbac.yaml:16-23` |
| ClusterRole | `ebs-external-attacher-role`, `ebs-csi-node-role`, `ebs-external-provisioner-role`, `ebs-external-resizer-role`, `ebs-external-snapshotter-role` | | `00-rbac.yaml:25`, `:65`, `:94`, `:184`, `:241` |
| ClusterRoleBinding | `ebs-csi-attacher-binding`, `ebs-csi-node-getter-binding`, `ebs-csi-provisioner-binding`, `ebs-csi-resizer-binding`, `ebs-csi-snapshotter-binding` | | `00-rbac.yaml:309-381` |
| Role / RoleBinding | `ebs-csi-leases-role` / `ebs-csi-leases-rolebinding` | `kube-system` | `00-rbac.yaml:384-417` |
| CSIDriver | `ebs.csi.aws.com` | | `00-rbac.yaml:421-430` |
| PodDisruptionBudget | `ebs-csi-controller` (maxUnavailable 1) | `kube-system` | `00-rbac.yaml:432-443` |
| Secret | `spinifex-gateway-ca` (shared name, see Conflicts) | `kube-system` | `10-controller.yaml:8-17` |
| Deployment | `ebs-csi-controller`, 2 replicas, preferred anti-affinity, tolerates `CriticalAddonsOnly` and `nvidia.com/gpu` | `kube-system` | `10-controller.yaml:19-82` |
| DaemonSet | `ebs-csi-node`, tolerates `nvidia.com/gpu` only | `kube-system` | `20-node.yaml:5-58` |
| StorageClass | `ebs-gp3`, default class, `WaitForFirstConsumer` | | `30-storageclass.yaml:5-18` |

Images: `public.ecr.aws/ebs-csi-driver/aws-ebs-csi-driver:v1.40.1` (`10-controller.yaml:89`, `20-node.yaml:67`), `public.ecr.aws/eks-distro/kubernetes-csi/external-provisioner:v5.2.0-eks-1-32-7` (`:147`), `external-attacher:v4.8.1-eks-1-32-7` (`:180`), `external-snapshotter/csi-snapshotter:v8.2.1-eks-1-32-7` (`:209`), `external-resizer:v1.13.2-eks-1-32-7` (`:238`), `livenessprobe:v2.14.0-eks-1-32-7` (`:270`, `20-node.yaml:155`), `node-driver-registrar:v2.13.0-eks-1-32-7` (`20-node.yaml:118`).
The bundle header says it is vendored from upstream v1.41.0 while the catalogue version is 1.40.1 (`00-rbac.yaml:1-3`); the driver image is v1.40.1.

### Identity

| Question | Answer |
|---|---|
| Takes `serviceAccountRoleArn` | Yes (`RequiresIRSA`), not required at create. |
| Annotated SA | `ebs-csi-controller-sa` (`00-rbac.yaml:12-13`, quoted, so an empty role renders `''`). |
| Injected pod | Controller Deployment only: `{{IRSA_ENV}}` (`10-controller.yaml:110`), `{{IRSA_VOLUME_MOUNT}}` (`:114`), `{{IRSA_VOLUME}}` (`:289`). The node DaemonSet gets no credentials and makes no EC2 calls (`20-node.yaml:1-3`). |
| SA owner | The bundle creates both ServiceAccounts. |
| Without a role | Controller uses node instance-profile credentials over IMDS. |
| Evidence | `tests/e2e/eks/eks_test.go:658-800` installs it with a role, creates the OIDC provider and role, and drives a PVC through CreateVolume/AttachVolume. |

### Readiness

| Question | Answer |
|---|---|
| Primary path | `ready` as soon as K3s records GVKs; neither replica need be running. The e2e waits for `ACTIVE` and then separately proves the data path (`eks_test.go:767-800`). |
| Fallback | Controller Deployment 2 of 2 available and node DaemonSet `desired > 0` and all ready. |
| Fallback false negative | A cluster with no worker node that tolerates scheduling (only the control plane, which carries `CriticalAddonsOnly` the DaemonSet does not tolerate) has `desired = 0`, so the fallback never passes. |
| False ready | Controller pending or crash-looping (including STS web-identity failures from a bad trust policy), or sidecars failing, still `ACTIVE` via the primary path. The bundle ships no `VolumeSnapshot` CRDs while running `csi-snapshotter`; the sidecar's behaviour without them was not verified and nothing in readiness would show it. |
| AWS comparison | `InsufficientNumberOfReplicas` and `AccessDenied` are the codes that would correspond to these states; neither is ever produced. |

### Removal

| Question | Answer |
|---|---|
| Deleted | All objects above, including the default StorageClass and the CSIDriver, in file order (RBAC and CSIDriver first, then Secret and controller, then node DaemonSet, then StorageClass). |
| Left behind (in cluster) | PersistentVolumes and PVCs it provisioned; `VolumeAttachment` objects; runtime leader-election Leases in `kube-system`; `CSINode` driver entries until node plugins unregister. |
| Left behind (outside cluster) | Spinifex EBS volumes backing those PVs, still allocated and possibly attached. |
| Stuck risk | PVs with `reclaimPolicy: Delete` and VolumeAttachments carry CSI sidecar finalizers that only the deleted controller clears, so later PVC deletion or node drain hangs and the backing volume is never deleted (upstream sidecar behaviour, not verified on this build). |
| Preserve | Keep the whole set; existing mounts and new provisioning keep working with the last-rendered role. |
| Completion signal | All 20 rendered objects NotFound (all in `kube-system` or cluster-scoped, so there is no namespace to watch); a meaningful "removed" additionally needs zero `VolumeAttachment`s and PVs with `spec.csi.driver == ebs.csi.aws.com`, or an explicit statement that those are user data that outlive the add-on. |

### Conflicts and configuration

| Object | Collides with |
|---|---|
| `kube-system/spinifex-gateway-ca` | The identical name in `aws-load-balancer-controller` (`10-aws-load-balancer-controller.yaml:33-41`). Deleting either add-on deletes the Secret the other mounts; K3s re-applies the survivor's file only when it changes, so the survivor's next pod start fails until then (re-apply behaviour not verified). A removal check by NotFound on this Secret also never completes while the other add-on re-creates it. |
| `StorageClass/ebs-gp3` with `is-default-class` | Any user default StorageClass, giving two defaults; K3s `local-storage` is disabled (`scripts/images/eks-node/setup.sh:98-101`). |
| `CSIDriver/ebs.csi.aws.com`, the five ClusterRoles, `ebs-csi-*` names | A self-managed (Helm) EBS CSI install, the `resolveConflicts` case; DeleteAddon would remove the user's driver too. |

No configuration hook; upstream exposes replica count, tolerations and StorageClass parameters through Helm values.

### Gaps

1. `ACTIVE` does not imply a running controller or working credentials.
2. Removal orphans PVs, VolumeAttachments and Spinifex volumes, and can wedge them behind finalizers.
3. Shares `spinifex-gateway-ca` with the load balancer controller, so removal of either is not isolated.
4. Bundle provenance comment (v1.41.0) disagrees with the catalogue version (1.40.1).

## 7. `aws-load-balancer-controller` 2.11.0

### Objects

| Kind | Name | Namespace | Source |
|---|---|---|---|
| CustomResourceDefinition | `ingressclassparams.elbv2.k8s.aws` | | `aws-load-balancer-controller/2.11.0/00-crds.yaml:1-8` |
| CustomResourceDefinition | `targetgroupbindings.elbv2.k8s.aws` | | `00-crds.yaml:256-263` |
| ServiceAccount | `aws-load-balancer-controller` (role annotation, unquoted) | `kube-system` | `10-aws-load-balancer-controller.yaml:2-13` |
| Secret | `aws-load-balancer-webhook-tls` (node-minted cert) | `kube-system` | `10-…yaml:17-28` |
| Secret | `spinifex-gateway-ca` (shared name) | `kube-system` | `10-…yaml:32-41` |
| Role / RoleBinding | `aws-load-balancer-controller-leader-election-role` / `-rolebinding` | `kube-system` | `10-…yaml:44-48`, `:242-254` |
| ClusterRole / ClusterRoleBinding | `aws-load-balancer-controller-role` / `-rolebinding` | | `10-…yaml:85-89`, `:258-269` |
| Service | `aws-load-balancer-webhook-service` | `kube-system` | `10-…yaml:273-277` |
| Deployment | `aws-load-balancer-controller`, 1 replica, tolerates `CriticalAddonsOnly` and `nvidia.com/gpu` | `kube-system` | `10-…yaml:288-372` |
| List → IngressClassParams | `alb` (subnets injected from `EKS_ELB_SUBNET_IDS`) | | `10-…yaml:375-386`, `sync.sh:197-215` |
| List → IngressClass | `alb` | | `10-…yaml:388-400` |
| MutatingWebhookConfiguration | `aws-load-balancer-webhook`: `mservice` (Services, all namespaces), `mpod` (Pods in namespaces labelled `elbv2.k8s.aws/pod-readiness-gate-inject`), `mtargetgroupbinding`; all `failurePolicy: Fail` | | `10-…yaml:402-488` |
| ValidatingWebhookConfiguration | `aws-load-balancer-webhook`: `vingressclassparams`, `vtargetgroupbinding`, `vingress`; all `failurePolicy: Fail` | | `10-…yaml:489-564` |

Image: `public.ecr.aws/eks/aws-load-balancer-controller:v2.11.0` (`10-…yaml:320`).
Webhook SANs come from `webhook.conf` (`aws-load-balancer-controller/2.11.0/webhook.conf`).

### Identity

| Question | Answer |
|---|---|
| Takes `serviceAccountRoleArn` | Yes (`RequiresIRSA`), not required at create. |
| Annotated SA | `aws-load-balancer-controller` (`10-…yaml:11-12`); the placeholder is unquoted, so the raw ARN becomes a YAML scalar and an empty role renders a null annotation value. |
| Injected pod | Controller Deployment: `{{IRSA_ENV}}` (`:319`), `{{IRSA_VOLUME_MOUNT}}` (`:349`), `{{IRSA_VOLUME}}` (`:372`); STS, EC2, ELBv2 and ACM endpoints pinned to the gateway by args (`:309-314`). |
| SA owner | The bundle. |
| Without a role | Node instance-profile credentials over IMDS. |
| Evidence | No e2e installs this add-on; IRSA is proven only for EBS CSI and a hand-written pod (`tests/e2e/eks/eks_test.go:234-247`). |

### Readiness

| Question | Answer |
|---|---|
| Primary path | `ready` when GVKs are recorded; the controller need not be running. |
| Fallback | The one Deployment at 1 available replica. |
| False ready | Controller pending, crash-looping, or failing STS/ELBv2 calls is still `ACTIVE`. Worse than a passive false ready: with the controller absent, the six `failurePolicy: Fail` webhooks reject every Service create or update cluster-wide (`mservice` matches all namespaces except LBC's own objects) and every Ingress, so an `ACTIVE` add-on can break unrelated workloads. CRD `Established` is not checked. |
| AWS comparison | `AdmissionRequestDenied` and `InsufficientNumberOfReplicas` are the matching issue codes; neither is produced. |

### Removal

| Question | Answer |
|---|---|
| Deleted | CRDs first (00 file sorts first), then ServiceAccount, Secrets, RBAC, Service, Deployment, IngressClassParams, IngressClass, and the webhook configurations last. |
| Left behind (in cluster) | User Ingresses and `LoadBalancer` Services it reconciled, with the controller's finalizers still set; the runtime leader Lease. |
| Left behind (outside cluster) | Spinifex ELBv2 load balancers, target groups, listeners and any security groups the controller created for those Ingresses and Services. |
| Stuck risk | Deleting the `targetgroupbindings` CRD waits on TargetGroupBindings whose finalizer only the controller clears; user Ingresses and Services keep finalizers no one removes, so they cannot be deleted (upstream controller behaviour, not verified on this build). Between the Deployment deletion and the webhook deletion, Service and Ingress writes fail; if the webhook delete errors, that state persists. |
| Preserve | Keep the whole set; the webhook cert and gateway CA must stay valid (cert is 10-year). |
| Completion signal | All rendered objects NotFound plus both CRDs gone (no `Terminating`); a meaningful "removed" also needs no remaining object carrying the controller's finalizers and no remaining Spinifex ELBv2 resources tagged for this cluster, which only the host side can observe. |

### Conflicts and configuration

| Object | Collides with |
|---|---|
| `kube-system/spinifex-gateway-ca` | `aws-ebs-csi-driver` (Section 6). |
| `IngressClass/alb`, `IngressClassParams/alb` | Any user IngressClass named `alb`, including a self-managed LBC. |
| CRDs, ClusterRole, webhook configuration names | A self-managed LBC (Helm), the `resolveConflicts` case. |

No configuration hook; `EKS_ELB_SUBNET_IDS` is the only per-cluster input and it comes from the node environment, not from `configurationValues`.
The design draft names this bundle as the first configuration target.

### Gaps

1. `ACTIVE` can coexist with fail-closed webhooks that have no backend, which breaks Service and Ingress writes cluster-wide.
2. Removal orphans user Ingresses/Services behind finalizers and Spinifex ELBv2 resources outside the cluster; nothing in the guest can observe the latter.
3. Shares `spinifex-gateway-ca` with EBS CSI.
4. Unquoted role placeholder takes the raw ARN as YAML.
5. No e2e coverage.

## 8. `nvidia-device-plugin` 0.17.4

### Objects

| Kind | Name | Namespace | Source |
|---|---|---|---|
| DaemonSet | `nvidia-device-plugin-daemonset`; nodeSelector `nvidia.com/gpu.present=true`; tolerates `nvidia.com/gpu`; `runtimeClassName: nvidia`; hostPath `/` read-only, `/var/lib/kubelet/device-plugins`, `/var/run/cdi` | `kube-system` | `nvidia-device-plugin/0.17.4/10-daemonset.yaml:14-75` |

Image: `nvcr.io/nvidia/k8s-device-plugin:v0.17.4` (`10-daemonset.yaml:41`).
Depends on a `RuntimeClass` named `nvidia` and on the node label written by `mulga-eks-provider-id` (`10-daemonset.yaml:1-12`); neither is in the bundle.

### Identity

No IRSA markers and no ServiceAccount object: pods run as the `kube-system/default` ServiceAccount that Kubernetes creates.
`EnsureGPUDevicePlugin` stages it with no role (`owner.go:79-89`); a user-created one may carry an inert role.

### Readiness

| Question | Answer |
|---|---|
| Primary path | `ready` when K3s records the DaemonSet GVK, regardless of GPU nodes. |
| Fallback | Requires `desiredNumberScheduled > 0`: never passes on a cluster with no GPU node. |
| False ready | `ACTIVE` with zero GPU nodes (nothing serving); `ACTIVE` after every GPU node group is deleted; `FAIL_ON_INIT_ERROR=false` (`10-daemonset.yaml:46-47`) and no readiness probe mean a pod whose NVML init failed is Ready while advertising no `nvidia.com/gpu`. The e2e proves the real signal separately: DaemonSet `1=1` and node allocatable `nvidia.com/gpu=1` (`tests/e2e/gpu/eks_gpu_test.go:260-275`). |

### Removal

| Question | Answer |
|---|---|
| Deleted | The DaemonSet. |
| Left behind | CDI spec files the plugin wrote to host `/var/run/cdi`; node `nvidia.com/gpu` capacity until kubelet reconciles the unregistered plugin (kubelet behaviour not verified). |
| Re-staging | Deleting it while GPU node groups exist leaves GPUs unschedulable until the next GPU node group create re-stages it; deleting the last GPU node group never unstages it (`eks-addon-lifecycle.md` row Cross-resource dependency). |
| Preserve | Keep the DaemonSet; nothing dangles. |
| Completion signal | DaemonSet NotFound; pods gone from GPU nodes. |

### Conflicts and configuration

A user-installed NVIDIA device plugin or GPU Operator would run a second plugin on the same nodes and contend for the kubelet device-plugin socket; the DaemonSet name matches the upstream static manifest, so a self-managed upstream install is overwritten.
No configuration hook (device-list strategy, time-slicing and MIG are upstream options).

### Gaps

1. Ready is indifferent to whether any GPU is exposed; the workload fallback is unreachable on non-GPU clusters.
2. Ownership is split between the auto-stager and the user, with no record of which.

## 9. Cross-bundle findings

| # | Finding | Bundles | Evidence |
|---|---|---|---|
| X1 | Readiness is "applied" on the primary path; workload state is never consulted while `gvks` is present, and runtime failure never yields `failed`. | all | `sync.sh:361-365`, `sync.sh:87`, `sync.sh:112` |
| X2 | The fallback silently drops absent objects (`--ignore-not-found`) and never checks CRD `Established`. | argocd, LBC, EBS | `sync.sh:304-319` |
| X3 | DaemonSet `desired > 0` makes the fallback unreachable for node-dependent bundles on clusters without matching nodes, while the primary path reports ready for the same state. | EBS, nvidia | `sync.sh:317` |
| X4 | Removal selects only objects in the rendered file; runtime-created and user-created dependants (PVs, VolumeAttachments, Applications, Ingresses with finalizers, Leases) are not inventoried, and out-of-cluster resources (EBS volumes, ELBv2) are invisible to the guest. | EBS, LBC, argocd | `sync.sh:397-401` |
| X5 | `kube-system/spinifex-gateway-ca` is rendered by two bundles under one name; either removal deletes the other's dependency, and a NotFound completion check on it is ill-defined while both exist. | LBC, EBS | `10-aws-load-balancer-controller.yaml:32-41`, `aws-ebs-csi-driver/1.40.1/10-controller.yaml:8-17` |
| X6 | No bundle has a stable ownership label set: only noop and the argocd Namespace carry `managed-by: spinifex-addon`; LBC, EBS and nvidia carry only upstream `app.kubernetes.io/name` labels, which a self-managed install of the same software carries too. A label-based inventory or conflict check has nothing reliable to select on. | all | per-bundle sources |
| X7 | Identity association is only the rendered pod env plus user-owned IAM objects; there is nothing Spinifex-owned to remove or observe, and the SA annotation is not acted on by any webhook. | LBC, EBS | Section 3.2 |
| X8 | `RequiresIRSA` is advisory: no create-time check either way, so a role on argocd, nvidia or noop is stored and inert. | all | `addons.go:61-99`, `addons.go:255` |
| X9 | The role ARN is substituted raw into sed and YAML; only `iam:PassRole` (which requires the role to exist) constrains it. | LBC, EBS | `sync.sh:231`, `passrole.go:31-37` |
| X10 | `configurationValues` has no schema, no `DescribeAddonConfiguration`, and no render hook for any bundle. | all | `sync.sh:359` |
| X11 | Removal runs only on the primary server and only from the local rendered file; a replaced primary has no file and removes nothing. | all | `eks-node-role.sh:138-142`, `sync.sh:386` |
| X12 | E2E covers create, ready and delete only for noop, create and data path for EBS (delete only in cleanup, not asserted), and DaemonSet rollout for nvidia; argocd and LBC have none. | all | `tests/e2e/eks/eks_test.go:158-247`, `tests/e2e/gpu/eks_gpu_test.go:260-275` |

## 10. Per-bundle observable removal signal (summary)

| Bundle | Objects that must be NotFound | Finalizer or dependant to watch | Out-of-cluster residue |
|---|---|---|---|
| `spinifex-noop` | Namespace `spinifex-noop` | Namespace `kubernetes` finalizer (aggregated API availability) | none |
| `argocd` | Namespace `argocd`, 3 CRDs, 3 ClusterRoles, 3 ClusterRoleBindings | `Application` resources finalizer; CRD and namespace `Terminating` | none from the bundle; user workloads it deployed |
| `aws-ebs-csi-driver` | 20 objects, all `kube-system` or cluster-scoped | PV and VolumeAttachment CSI finalizers | Spinifex EBS volumes |
| `aws-load-balancer-controller` | 2 CRDs plus 13 objects, webhooks included | Ingress, Service and TargetGroupBinding controller finalizers; CRD `Terminating` | Spinifex ELBv2 load balancers, target groups, security groups |
| `nvidia-device-plugin` | 1 DaemonSet | none | host CDI spec files |
