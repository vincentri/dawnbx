# Feature Specification: SSH-Free AWS Control Plane

**Feature Branch**: `task/aws-control-plane`

**Created**: 2026-09-26

**Status**: Draft

**Input**: User description: "Build an SSH-free, UI-driven AWS cluster provisioning control plane for dawnbx."

## Clarifications

### Session 2026-09-26

- Q: For phase one, can one dawnbx control plane manage clusters in more than one AWS account? → A: One AWS account and one active standard credential source per control plane.
- Q: If AWS provisioning fails after paid resources have started, should dawnbx automatically remove those resources? → A: Automatically remove resources created by the failed provisioning operation.
- Q: What must the phase-one price estimate include before an operator creates a cluster? → A: Estimated hourly and monthly compute, storage, and public-IP charges; clearly exclude variable data transfer, taxes, and provider discounts.
- Q: Should phase one keep the control plane private and have it poll AWS for provisioning status? → A: Keep the control plane private and poll AWS stack and instance status using its configured AWS credential source.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Create an AWS Cluster Without SSH (Priority: P1)

An authorized operator starts the dawnbx control plane on a machine that does not
host a cluster, configures an AWS cluster through the dashboard, sees the price
estimate, explicitly confirms the purchase, and receives a usable cluster URL
without opening an SSH session.

**Why this priority**: This is the core product outcome. Without it, operators
still need infrastructure expertise and out-of-band host access for routine
cluster creation.

**Independent Test**: Start the control plane without a local cluster or volume
marker, create a cluster through the dashboard with valid AWS access, and use
the resulting URL to reach the cluster without SSH.

**Acceptance Scenarios**:

1. **Given** the control plane has no local cluster or volume marker, **When**
   an authorized operator opens its dashboard, **Then** they can begin creating
   an AWS cluster.
2. **Given** the operator selects AWS and supplies a valid credential source,
   **When** they choose a region, capacity, storage, and optional domain,
   **Then** the dashboard shows the selected configuration and an estimated
   price before any paid resource is created.
3. **Given** the operator has reviewed the estimate, **When** they explicitly
   confirm creation, **Then** the system begins provisioning and does not
   require the operator to use SSH.
4. **Given** provisioning succeeds, **When** the dashboard reports the cluster
   ready, **Then** it shows the cluster URL and lets the operator access its
   initial credentials through the control plane.

---

### User Story 2 - Understand and Recover From Provisioning Status (Priority: P2)

An operator can follow a cluster's provisioning progress, understand a failure,
and take the stated next action without inspecting a host over SSH.

**Why this priority**: A UI-only creation flow is not operationally useful if a
failure sends the operator back to shell access or opaque provider consoles.

**Independent Test**: Trigger successful and failed provisioning attempts and
verify that the dashboard identifies the current phase, final result, relevant
error, and the next supported action for each.

**Acceptance Scenarios**:

1. **Given** a cluster is provisioning, **When** its state changes, **Then**
   the dashboard displays the current stage and whether operator action is
   required.
2. **Given** provisioning fails, **When** the operator views the cluster,
   **Then** the dashboard explains the failure without exposing secret values
   and identifies the next supported action.
3. **Given** a host becomes unusable after creation, **When** the dashboard
   cannot recover it, **Then** it clearly marks SSH as a rescue-only operation
   rather than a required normal workflow.
4. **Given** the control plane has no public inbound route, **When** an AWS
   provisioning operation changes state, **Then** the dashboard receives the
   updated status without an SSH session or a host-to-control-plane connection.

---

### User Story 3 - Scale a Cluster From the Dashboard (Priority: P3)

An operator adds a worker node to a ready cluster and removes an unused worker
node from the dashboard, without managing host credentials or issuing join
commands manually.

**Why this priority**: Node lifecycle is the next routine operation after
cluster creation; requiring SSH here would break the advertised user experience.

**Independent Test**: On a ready cluster, add a worker, observe it become
available, then remove a worker that holds no sandboxes and observe that it no
longer belongs to the cluster.

**Acceptance Scenarios**:

1. **Given** a ready cluster, **When** the operator chooses worker capacity and
   confirms an add-node action, **Then** the dashboard begins provisioning the
   worker and shows its status.
2. **Given** a ready worker holds no sandboxes, **When** the operator confirms
   its removal, **Then** the system removes it from the cluster and reports the
   final result.
3. **Given** a worker holds one or more sandboxes, **When** the operator tries
   to remove it, **Then** the dashboard blocks removal and explains why.

---

### User Story 4 - Choose a Supported Provider (Priority: P3)

An operator sees AWS as the supported phase-one provider and sees GCP and Azure
as future options without being able to begin unsupported provisioning flows.

**Why this priority**: The product must make its current capability unambiguous
while preserving a consistent experience for later providers.

**Independent Test**: Open the provider picker and verify that AWS can be
selected, while GCP and Azure are visible, labelled unavailable, and cannot
start provisioning.

**Acceptance Scenarios**:

1. **Given** an operator opens provider selection, **When** they view the
   choices, **Then** AWS is available and GCP and Azure are visibly unavailable.
2. **Given** the operator chooses a future provider, **When** they attempt to
   continue, **Then** no paid resource or provisioning request is created.

---

### Edge Cases

- AWS access is valid but lacks a required permission: explain the missing
  capability without revealing credentials or starting a partial request.
- The selected region or capacity is unavailable: retain the operator's choices,
  report the constraint, and require a revised selection before confirmation.
- The estimated price changes before confirmation: present the updated estimate
  and require a new explicit confirmation.
- Provisioning becomes unreachable or exceeds its expected duration: mark the
  state as requiring attention and preserve enough non-secret status for the
  operator to determine the next supported action.
- When provisioning fails after creating paid resources, remove the resources
  created exclusively for that failed operation while retaining non-secret
  diagnostics in the control plane.

- A credential becomes invalid after cluster creation: preserve the running
  cluster and identify that new provider actions need renewed access.
- When a control-plane administrator rotates a cluster credential, revoke the
  superseded credential and retain no secret value in the resulting status.

- The cluster URL is unavailable at the reported-ready point: do not report the
  cluster ready; show the failed readiness state instead.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The system MUST provide a control-plane mode that starts and
  serves its dashboard and management functions without a local cluster or
  local volume marker.
- **FR-002**: The system MUST let an authorized operator select AWS as the
  phase-one provider and MUST show GCP and Azure as unavailable future options
  that cannot initiate provisioning.
- **FR-003**: The system MUST let an operator use a standard AWS credential
  source for the control plane's one configured AWS account and MUST NOT require
  pasting long-lived access keys into the dashboard.
- **FR-004**: The system MUST let an operator choose an AWS region, instance
  capacity, storage amount, and optional domain before creating a cluster.
- **FR-005**: Before creating any paid resource, the system MUST display
  estimated hourly and monthly compute, storage, and public-IP charges for the
  selected configuration, clearly state that data transfer, taxes, and provider
  discounts are excluded, and require explicit operator confirmation.

- **FR-006**: The system MUST create and retain cluster API and administrator
  credentials across control-plane restarts. Only control-plane administrators
  MAY reveal or rotate them through the dashboard.
- **FR-007**: The system MUST transfer initial cluster credentials during
  provisioning without exposing their values in resource outputs, logs,
  support material, or screenshots.
- **FR-008**: The system MUST show each cluster's configuration, provisioning
  phase, current status, and non-secret failure explanation in the dashboard.
- **FR-009**: The system MUST show a usable cluster URL only after the cluster
  is ready to accept direct client connections.
- **FR-010**: The system MUST let an operator add a worker node through the
  dashboard and show the node's provisioning and ready status.
- **FR-011**: The system MUST let an operator remove a worker node only when it
  contains no sandboxes, and MUST explain a blocked removal.
- **FR-012**: The system MUST make normal cluster creation and node lifecycle
  possible without SSH. It MAY direct an operator to SSH only for an explicitly
  identified host-rescue condition.
- **FR-013**: The system MUST represent credential discovery, cluster creation,
  cluster status, node addition, node removal, and cluster deletion as
  provider-neutral lifecycle capabilities before adding provider-specific
  behavior.
- **FR-014**: The system MUST ensure that SDKs and the CLI connect directly to
  a cluster URL and do not route routine sandbox operations through the control
  plane.
- **FR-015**: The phase-one release MUST use the existing AWS provisioning path
  for the initial cluster host and MUST not add GCP or Azure provisioning.
- **FR-016**: When provisioning fails, the system MUST automatically remove
  paid resources created exclusively for that failed operation and retain
  non-secret diagnostics for the operator.
- **FR-017**: The system MUST obtain AWS provisioning status through the
  control plane's configured credential source and MUST NOT require the control
  plane to accept public inbound status reports from provisioned hosts.
- **FR-018**: The system MUST let an authorized operator delete a cluster from
  the dashboard, MUST require an explicit confirmation, and MUST refuse while
  workers are still attached. It MUST remove the cluster's paid resources and
  MUST NOT delete the record until the provider confirms they are gone.

**Why FR-018 exists.** The assumption below scoped the delete *dashboard flow*
out, "unless later planning includes it". Building it proved that leaving it out
is not neutral: a cluster that failed to provision keeps its name and its record
forever, and the failure panel's only advice is to delete it. So it is in, as
FR-018, and the assumption is amended to say so.

### Key Entities *(include if feature involves data)*

- **Control Plane**: The operator-facing management service that stores cluster
  records, initial credentials, provisioning state, and supported actions.
- **Provider Capability**: A provider-neutral description of what a cloud
  provider can do for credentials, clusters, nodes, status, and deletion.
- **Credential Source**: The one active, authorized source through which the
  control plane performs provider actions for its configured AWS account; it
  never stores pasted long-lived access keys.
- **Cluster**: A provisioned sandbox environment with a provider, configuration,
  lifecycle status, direct URL, retained initial credentials, and worker nodes.
- **Worker Node**: A cluster member that can host sandboxes and has a lifecycle
  status independent of the control plane.
- **Provisioning Operation**: A tracked create, add-node, or remove-node action
  with a current phase, result, and non-secret diagnostic information.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In 100% of scripted first-run tests, an operator can start the
  control plane and reach the cluster-creation dashboard without a local cluster
  or volume marker.
- **SC-002**: In 100% of scripted successful AWS creation tests, the operator
  completes provider selection, configuration, price review, and confirmation
  without SSH and receives a directly usable cluster URL.
- **SC-003**: The dashboard reflects a provisioning or node-status transition
  within 60 seconds of that transition being known to the system in at least 95%
  of test runs.
- **SC-004**: In 100% of scripted provisioning-failure tests, an operator can
  identify the failed phase and the next supported action from the dashboard
  without SSH or a secret value.
- **SC-005**: In usability testing with five representative operators, at least
  four complete the AWS cluster-creation flow on their first attempt without
  needing an SSH instruction.
- **SC-006**: In provider-selection tests, no GCP or Azure resource is created
  or provisioning attempt started.
- **SC-007**: In automated creation and failure-path checks, no initial cluster
  API key or administrator password appears in captured logs, resource outputs,
  or dashboard error messages.
- **SC-008**: In 100% of scripted provisioning-failure tests that create paid
  resources, the system completes cleanup of resources created exclusively for
  the failed operation and retains the non-secret diagnostic result.
- **SC-009**: In 100% of pre-purchase configuration tests, the estimate shows
  its hourly and monthly fixed-charge breakdown and states its excluded variable
  charges before the operator can confirm creation.
- **SC-010**: In 100% of scripted provisioning tests with no public inbound
  route to the control plane, the dashboard receives the final provisioning
  result and its non-secret diagnostic information.

## Assumptions

- The existing AWS provisioning path remains the phase-one foundation and is
  available to the control plane once a release artifact is hosted.
- An authorized operator means an existing control-plane administrator. This
  feature preserves the product's current administrator-only management model.
- Standard AWS credential discovery and scoped roles are supported; local named
  profiles are a convenience, not the sole credential model.
- Phase one supports one configured AWS account and one active standard
  credential source per control plane; multi-account management is outside this
  feature.
- The control plane has outbound HTTPS access to AWS service endpoints; it does
  not require a public inbound route for provisioning-status reporting.
- AWS is the only provider that creates infrastructure in phase one. GCP and
  Azure are deliberately visible but disabled, not partial implementations.
- SSH and other out-of-band host access remain available only for rescue work,
  such as a wedged node, full disk, or failed upgrade.
- Cluster deletion is included in the provider lifecycle boundary, and the
  dashboard flow for it is now in scope as FR-018. It was originally excluded on
  the understanding that the failure panel would have another way to suggest;
  it does not.
