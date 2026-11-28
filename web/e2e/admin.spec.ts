import { expect, test } from "@playwright/test";
import { API_URL, MOCK_URL } from "./ports";
import { call, newTenant, signIn } from "./support";

async function seedSuggestion(tenant: string, eventType: string): Promise<void> {
  const res = await fetch(`${MOCK_URL}/v1/learning/suggestions`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      tenantId: tenant,
      eventPattern: { eventType, label: null },
      currentAdjustment: 0,
      proposedAdjustment: -5,
      signalCount: 9,
      rationale: "People dismiss these.",
    }),
  });
  if (!res.ok) throw new Error(`seed suggestion: ${res.status} ${await res.text()}`);
}

test("approve a learning suggestion", async ({ page }) => {
  const t = await newTenant();
  await seedSuggestion(t.tenant, "task.overdue");

  await signIn(page, t.admin, "/admin");
  const card = page.getByRole("article", { name: /Suggestion for Notifications of type task.overdue/ });
  await expect(card.getByText("Evidence: 9 feedback signals.")).toBeVisible();
  await expect(card.getByLabel("Rationale")).toHaveText("People dismiss these.");
  await card.getByRole("button", { name: "Approve" }).click();
  await expect(page.getByText(/Approved: Notifications of type task.overdue/)).toBeVisible();
  await expect(page.getByText("No suggestion is waiting for a decision.")).toBeVisible();

  const list = await call<{ suggestions: { status: string }[] }>(t.admin, "GET", "/api/v1/admin/learning/suggestions");
  expect(list.suggestions.map((s) => s.status)).toEqual(["APPROVED"]);
});

test("a member who opens an admin URL is turned away, and the server refuses the data", async ({ page }) => {
  const t = await newTenant();
  await signIn(page, t.member, "/admin/learning");
  await expect(page).toHaveURL(/\/tasks$/);
  await expect(page.getByRole("link", { name: "Admin" })).toHaveCount(0);
  const res = await fetch(`${API_URL}/api/v1/admin/learning/suggestions`, { headers: { Authorization: `Bearer ${t.member}` } });
  expect(res.status).toBe(403);
});
