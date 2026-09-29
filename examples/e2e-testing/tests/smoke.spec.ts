import { test, expect } from "@playwright/test";

// A minimal starter smoke test — replace with your own. It runs against
// $TARGET_URL and captures a screenshot, video, trace, and any console/network
// errors for the report. Point run-e2e.sh at any URL to try it.
const TARGET = process.env.TARGET_URL || "";

test("page loads without errors", async ({ page }) => {
  const consoleErrors: string[] = [];
  page.on("console", (m) => m.type() === "error" && consoleErrors.push(m.text()));
  page.on("pageerror", (e) => consoleErrors.push(String(e)));

  const resp = await page.goto(TARGET, { waitUntil: "networkidle" });
  expect(resp?.status(), "page should return a success status").toBeLessThan(400);

  const title = await page.title();
  expect(title.length, "page should have a non-empty <title>").toBeGreaterThan(0);

  // fail the test if the page logged JS errors
  expect(consoleErrors, `console/JS errors on ${TARGET}`).toEqual([]);
});

test("has visible content (not blank)", async ({ page }) => {
  await page.goto(TARGET, { waitUntil: "domcontentloaded" });
  const text = (await page.evaluate(() => document.body?.innerText || "")).trim();
  expect(text.length, "page renders visible text").toBeGreaterThan(20);
});
