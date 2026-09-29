// Baked example suite — `pwtest` runs this out of the box so a fresh sandbox
// produces a video, trace, and HTML report with zero setup.
const { test, expect } = require('@playwright/test');

test('example.com loads and links to IANA', async ({ page }) => {
  await page.goto('https://example.com');
  await expect(page).toHaveTitle(/Example Domain/);
  await page.click('a');
  await expect(page).toHaveURL(/iana/);
});
