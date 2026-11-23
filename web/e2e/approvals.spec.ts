import { expect, test } from "@playwright/test";
import { call, createTask, newTenant, proposePriority, scriptLLM, signIn, token } from "./support";

const getTask = (t: { member: string }, id: string) => call<{ priority: string; version: number; description: string }>(t.member, "GET", `/api/v1/tasks/${id}`);

test("approve a proposal: the change is applied and the inbox empties", async ({ page }) => {
  const t = await newTenant();
  const { task } = await proposePriority(t, "Payments are down", "p1_critical", "Payments are down for everyone.");

  await signIn(page, t.member, "/approvals");
  const card = page.getByRole("article", { name: "Proposal from Triage agent" });
  await expect(card.getByRole("link", { name: "Payments are down" })).toBeVisible();
  // Current beside proposed, with the agent's reasoning.
  const row = card.getByRole("row", { name: /Priority/ });
  await expect(row.getByRole("cell").nth(0)).toHaveText("P3 Medium");
  await expect(row.getByRole("cell").nth(1)).toHaveText("P1 Critical");
  await expect(card.getByLabel("Reasoning")).toHaveText("Payments are down for everyone.");
  expect((await getTask(t, task.id)).priority).toBe("p3_medium"); // nothing happens before the decision

  await card.getByRole("button", { name: "Approve" }).click();
  await expect(page.getByText(/Approved and applied: Triage agent's proposal for "Payments are down"/)).toBeVisible();
  await expect(page.getByText("Nothing is waiting for your decision.")).toBeVisible();
  expect((await getTask(t, task.id)).priority).toBe("p1_critical");

  // The task page names the agent and its reasoning.
  await page.getByRole("link", { name: "Tasks" }).click();
  await page.getByRole("link", { name: "Payments are down" }).click();
  const activity = page.getByRole("list", { name: "Agent activity" });
  await expect(activity.getByRole("heading", { name: /Triage agent/ })).toBeVisible();
  await expect(activity.getByText("Update task details")).toBeVisible();
  await expect(activity.getByLabel("Reasoning")).toHaveText("Payments are down for everyone.");
});

test("reject a proposal: nothing is applied", async ({ page }) => {
  const t = await newTenant();
  const { task, job } = await proposePriority(t, "Rename the button", "p1_critical", "Looks urgent.");

  await signIn(page, t.member, "/approvals");
  await page.getByRole("article", { name: "Proposal from Triage agent" }).getByRole("button", { name: "Reject" }).click();
  await expect(page.getByText(/Rejected: Triage agent's proposal for "Rename the button"/)).toBeVisible();
  await expect(page.getByText("Nothing is waiting for your decision.")).toBeVisible();
  expect((await getTask(t, task.id)).priority).toBe("p3_medium");
  expect((await call<{ status: string }>(t.member, "GET", `/api/v1/agent-jobs/${job.id}`)).status).toBe("dismissed");
});

test("a stale proposal says so, and is not applied", async ({ page }) => {
  const t = await newTenant();
  const { task } = await proposePriority(t, "Old news", "p1_critical", "Seems urgent.");
  await signIn(page, t.member, "/approvals");
  const card = page.getByRole("article", { name: "Proposal from Triage agent" });
  await expect(card.getByRole("button", { name: "Approve" })).toBeVisible();

  // Someone edits the task after the agent looked at it.
  const now = await getTask(t, task.id);
  await call(t.member, "PATCH", `/api/v1/tasks/${task.id}`, { description: "Edited meanwhile" }, { "If-Match": `"${now.version}"` });

  await card.getByRole("button", { name: "Approve" }).click();
  await expect(page.getByRole("alert").first()).toContainText("This proposal is out of date: the task changed after the agent made it");
  const after = await getTask(t, task.id);
  expect(after.priority).toBe("p3_medium");
  expect(after.description).toBe("Edited meanwhile");
  await expect(card).toHaveCount(0); // the proposal is closed
});

test("a person who may not decide is told so, and the proposal stays", async ({ page }) => {
  const t = await newTenant();
  const { job } = await proposePriority(t, "Not yours", "p1_critical", "Urgent.");
  // usr_eve is neither the creator, the assignee, nor an admin.
  const eve = token({ tenant: t.tenant, user: "usr_eve" });
  await signIn(page, eve, "/approvals");
  await page.getByRole("article", { name: "Proposal from Triage agent" }).getByRole("button", { name: "Approve" }).click();
  await expect(page.getByRole("alert").first()).toContainText("Only the task's assignee, its creator, or an admin can decide this proposal.");
  expect((await call<{ status: string }>(t.member, "GET", `/api/v1/agent-jobs/${job.id}`)).status).toBe("awaiting_approval");
});

test("decompose: the page shows progress, then the subtasks and their dependency graph", async ({ page }) => {
  const t = await newTenant();
  const task = await createTask(t, "Launch the store");
  await call(t.admin, "PATCH", `/api/v1/workspaces/${t.workspace}/settings`, {
    agent_settings: { agents_enabled: true, require_hitl_for_all: false, disabled_agents: ["triage", "blocker_investigator", "digest", "learning"] },
  });
  await scriptLLM(task.id, [
    { tool_calls: [{ name: "CreateSubtaskDAG", arguments: { parent_task_id: task.id, reasoning: "Two steps.", subtasks: [
      { title: "Build the catalogue" }, { title: "Open the doors", depends_on: ["Build the catalogue"] }] } }] },
    { content: "Split into two subtasks." },
  ]);

  await signIn(page, t.member, `/tasks/${task.id}`);
  await expect(page.getByText("No subtasks yet.")).toBeVisible();
  await page.getByRole("button", { name: "Decompose" }).click();
  await expect(page.getByRole("status", { name: "Decomposition progress" })).toBeVisible();
  const list = page.getByRole("list", { name: "Subtasks" });
  await expect(list.getByRole("link", { name: "Build the catalogue" })).toBeVisible({ timeout: 20_000 });
  await expect(list.getByText(/Open the doors.*waits for Build the catalogue/)).toBeVisible();
  await expect(page.getByRole("img", { name: "Dependency graph: 2 subtasks, 1 links" })).toBeVisible();
  await expect(page.getByRole("status", { name: "Decomposition progress" })).toContainText("is done");
  await expect(page.getByRole("list", { name: "Agent activity" }).getByText("Create subtasks")).toBeVisible();
});

test("the brief page, where a brief notification's view link points, says when there is no brief", async ({ page }) => {
  const t = await newTenant();
  await signIn(page, t.member, "/brief");
  await expect(page.getByRole("heading", { name: "Morning brief" })).toBeVisible();
  await expect(page.getByText(/You have no brief yet/)).toBeVisible();
});
