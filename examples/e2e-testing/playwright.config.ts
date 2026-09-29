import { defineConfig } from "@playwright/test";

// Records everything — screenshots, screen recording (video), and a trace for
// every test — so the HTML report is a complete record of the run.
export default defineConfig({
  testDir: "./tests",
  timeout: 60_000,
  expect: { timeout: 10_000 },
  retries: 1,
  reporter: [["html", { open: "never", outputFolder: "playwright-report" }], ["list"]],
  use: {
    baseURL: process.env.TARGET_URL,
    headless: true,
    screenshot: "on",
    video: "on",
    trace: "on",
  },
});
