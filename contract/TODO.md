# R2 gap checklist (M12-08)

M12 asks for a list of the agent endpoints, events, and tools still missing from the contract, ticked off as M5 to M11 land. M12 ran after R2 was complete, so nothing is missing: every item below was in the contract when M12 closed, and the open list is empty. `TestR2ChecklistIsEmpty` fails if an unticked item is added here.

## Open

None.

## Checked when M12 closed

Endpoints, in [openapi.yaml](openapi.yaml):

- [x] `POST /api/v1/tasks/{id}/decompose` (M8)
- [x] `GET /api/v1/agent-jobs/{id}`, `POST .../apply`, `POST .../dismiss` (M6)
- [x] `GET /api/v1/me/brief` (M10)
- [x] `GET` and `PUT /api/v1/me/contact` (AP-05)
- [x] `GET /api/v1/admin/learning/suggestions`, `POST .../{id}/decision` (M3, used by M11)
- [x] `GET` and `PATCH /api/v1/workspaces/{id}/settings` with `agent_settings` (M5)
- [x] `POST /api/v1/webhooks/agentpulse` with `approve`, `reject`, `unblock`, and `callbackId` (M6, M9, AP-06)
- [x] The R3 lists the clients need: `GET /agent-jobs`, `/tasks/{id}/subtasks`, `/tasks/{id}/agent-runs`, `/admin/outbox` and replay, `/me`, `id_prefix` (M12-04)

Events, in [events/](events/):

- [x] `task.unblocked`, `task.dependency_resolved`, `task.dependency_added`, `task.dependency_reopened` (M5)
- [x] `agent.approval_requested` (M6)
- [x] `task.escalated` (M9)
- [x] `digest.morning_brief` (M10)
- [x] `contact.update` (AP-05) and `learning.suggestion` (M11) outbox payloads

Tools, in [tools/](tools/):

- [x] `UpdateTaskAttributes`, `LinkDependencies` (M7)
- [x] `CreateSubtaskDAG` (M8)
- [x] `DraftResolutionComment`, `ReassignTask`, `ShiftDueDate`, `ArchiveTask` (M9)
- [x] `ProposeNotificationRule` (M11)

Not R2 gaps, and planned elsewhere: `GET /notifications/in-app` and `POST /notifications/{id}/ack` (M14-06), and `GET /events` (later, live updates). They join the contract, first, with their tasks.
