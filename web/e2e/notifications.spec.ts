import { expect, test } from "@playwright/test";
import { MOCK_PORT } from "./ports";
import { createTask, mock, newTenant, signIn } from "./support";

function item(id: string, title: string, taskId: string) {
  return { id, eventType: "task.updated", title, body: "Priority changed", createdAt: "2026-11-20T10:00:00Z", payload: { taskId } };
}

const feedbackCalls = async (id: string) =>
  (await mock.requests()).filter((r) => r.method === "POST" && r.path === "/v1/feedback" && r.body?.notificationId === id);

test("the feed shows the person's notifications, and dismissing one sends one feedback call; the browser never calls AgentPulse", async ({ page }) => {
  const t = await newTenant();
  const task = await createTask(t, "Fix checkout");
  await mock.seedFeed(t.tenant, t.memberId, [item("ntf-e2e-1", "Checkout changed", task.id)]);
  const toAgentPulse: string[] = [];
  page.on("request", (r) => {
    if (new URL(r.url()).port === String(MOCK_PORT)) toAgentPulse.push(r.url());
  });

  await signIn(page, t.member);
  await page.getByRole("link", { name: "Notifications" }).click();
  await expect(page.getByRole("heading", { name: "Checkout changed" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Fix checkout" })).toBeVisible();

  await page.getByRole("button", { name: "Dismiss" }).click();
  await expect(page.getByText(/Everything here is handled/)).toBeVisible();
  await expect.poll(async () => (await feedbackCalls("ntf-e2e-1")).length).toBe(1);
  expect((await feedbackCalls("ntf-e2e-1"))[0].body).toMatchObject({ notificationId: "ntf-e2e-1", userId: t.memberId, tenantId: t.tenant, signal: "dismissed" });
  expect(toAgentPulse).toEqual([]);
});

test("acting on a notification whose first forward fails still sends the feedback once it succeeds", async ({ page }) => {
  const t = await newTenant();
  const task = await createTask(t, "Retry me");
  await mock.seedFeed(t.tenant, t.memberId, [item("ntf-e2e-2", "Retry change", task.id)]);
  await mock.fail("POST", "/v1/feedback", 500, 1);

  await signIn(page, t.member, "/notifications");
  await page.getByRole("button", { name: "Open" }).click();
  await expect(page.getByRole("heading", { name: "Retry me" })).toBeVisible(); // the click did not wait on AgentPulse

  // One failed attempt and one that succeeded; then nothing more.
  await expect.poll(async () => (await feedbackCalls("ntf-e2e-2")).length).toBe(2);
  await page.waitForTimeout(1000);
  expect((await feedbackCalls("ntf-e2e-2")).length).toBe(2);
});

test("with AgentPulse unreachable the panel shows an error and the task screens still work", async ({ page }) => {
  const t = await newTenant();
  await createTask(t, "Still works");
  await mock.fail("GET", `/v1/tenants/${t.tenant}/users/${t.memberId}/in-app-feed`, 503, -1);

  await signIn(page, t.member, "/notifications");
  await expect(page.getByRole("alert")).toContainText("Notifications are unavailable right now");

  await page.getByRole("link", { name: "Tasks" }).click();
  await page.getByRole("link", { name: "Still works" }).click();
  await expect(page.getByRole("heading", { name: "Still works" })).toBeVisible();
  await page.getByLabel("Priority").selectOption("p2_high");
  await expect(page.getByText(/version 2/)).toBeVisible();
});
