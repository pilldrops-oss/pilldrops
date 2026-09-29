// Baked-in defaults: headless, record everything, artifacts land where the
// sandbox preview port serves them. Run tests with `pwtest` (or npx
// playwright test) — no per-project setup needed.
module.exports = {
  testDir: './tests',
  outputDir: '/root/recordings/test-results',
  reporter: [
    ['list'],
    ['html', { outputFolder: '/root/recordings/report', open: 'never' }],
  ],
  use: {
    // PW_HEADED=1 (set by the playwright-headed image, which bakes
    // PD_DESKTOP=1 so envd brings up the display) switches to headed mode;
    // default is headless.
    headless: !process.env.PW_HEADED,
    video: 'on',
    trace: 'on',
    screenshot: 'on',
  },
};
