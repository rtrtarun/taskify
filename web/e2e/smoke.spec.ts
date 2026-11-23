import { expect, test } from "@playwright/test";

test("the app loads", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Taskify" })).toBeVisible();
});
