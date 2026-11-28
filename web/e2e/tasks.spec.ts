import { expect, test } from "@playwright/test";
import { call, createTask, newTenant, signIn, token } from "./support";

test("create a task, edit it, comment, and complete it", async ({ page }) => {
  const t = await newTenant();
  await signIn(page, t.member);

  await page.getByLabel("Title").fill("Ship the release");
  await page.getByLabel("Workspace").fill(t.workspace);
  await page.getByRole("button", { name: "Add task" }).click();
  await page.getByRole("link", { name: "Ship the release" }).click();
  await expect(page.getByRole("heading", { name: "Ship the release" })).toBeVisible();

  await page.getByLabel("Priority").selectOption("p1_critical");
  await expect(page.getByText(/version 2/)).toBeVisible();

  await page.getByLabel("Description").fill("Cut the tag and publish.");
  await page.getByLabel("Labels (comma separated)").fill("release, ops");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByText(/version 3/)).toBeVisible();

  await page.getByRole("textbox", { name: "Comment" }).fill("Starting now <b>not bold</b>");
  await page.getByRole("button", { name: "Add comment" }).click();
  await expect(page.getByText("Starting now <b>not bold</b>")).toBeVisible();

  await page.getByRole("button", { name: "Complete" }).click();
  await expect(page.getByText(/Completed - version/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Complete" })).toHaveCount(0);

  // The server agrees with what the page shows.
  const list = await call<{ tasks: { title: string; status: string; priority: string; labels: string[] }[] }>(
    t.member, "GET", "/api/v1/tasks?status=completed");
  expect(list.tasks).toMatchObject([{ title: "Ship the release", status: "completed", priority: "p1_critical", labels: ["release", "ops"] }]);
});

test("two sessions edit one task: the second sees a conflict and loses nothing silently", async ({ browser }) => {
  const t = await newTenant();
  const task = await createTask(t, "Shared task");
  const a = await (await browser.newContext()).newPage();
  const b = await (await browser.newContext()).newPage();
  await signIn(a, t.member, `/tasks/${task.id}`);
  await signIn(b, t.member, `/tasks/${task.id}`);
  await expect(a.getByRole("heading", { name: "Shared task" })).toBeVisible();
  await expect(b.getByRole("heading", { name: "Shared task" })).toBeVisible();

  // Session A saves first.
  await a.getByLabel("Priority").selectOption("p2_high");
  await expect(a.getByText(/version 2/)).toBeVisible();

  // Session B, still on version 1, tries to change the same task.
  await b.getByLabel("Priority").selectOption("p1_critical");
  const notice = b.getByRole("alert");
  await expect(notice).toContainText("your change was not saved");
  await expect(notice).toContainText("priority: p1_critical"); // what B tried to save is shown, not dropped
  await expect(b.getByLabel("Priority")).toHaveValue("p2_high"); // rolled back and reloaded: A's value
  await expect(b.getByText(/version 2/)).toBeVisible();

  const latest = await call<{ priority: string; version: number }>(t.member, "GET", `/api/v1/tasks/${task.id}`);
  expect(latest).toMatchObject({ priority: "p2_high", version: 2 });
});

test("a user in one tenant cannot load another tenant's task URL", async ({ page }) => {
  const mine = await newTenant();
  const theirs = await newTenant();
  const secret = await createTask(theirs, "Other tenant's secret");

  await signIn(page, mine.member, `/tasks/${secret.id}`);
  await expect(page.getByRole("alert")).toContainText("does not exist, or you cannot see it");
  await expect(page.getByText("Other tenant's secret")).toHaveCount(0);
});

test("an expired session returns to sign-in", async ({ page }) => {
  const t = await newTenant();
  await signIn(page, token({ tenant: t.tenant, user: "usr_bob", ttlSeconds: 8 }));
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByRole("status")).toContainText("session expired");
});
