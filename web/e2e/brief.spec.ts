import { expect, test } from "@playwright/test";
import { newTenant, signIn } from "./support";

test("the brief page, where a brief notification's view link points, says when there is no brief", async ({ page }) => {
  const t = await newTenant();
  await signIn(page, t.member, "/brief");
  await expect(page.getByRole("heading", { name: "Morning brief" })).toBeVisible();
  await expect(page.getByText(/You have no brief yet/)).toBeVisible();
});
