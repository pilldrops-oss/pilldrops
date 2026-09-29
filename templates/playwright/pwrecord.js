// Record a headless visit: opens the URL, scrolls through the page for the
// requested duration, and saves a .webm under /root/recordings/videos where
// the preview port serves it (browsers play .webm inline).
const { chromium } = require('@playwright/test');
const fs = require('fs');

const [url, secsArg] = process.argv.slice(2);
const secs = Math.max(1, Number(secsArg || 10));

(async () => {
  const browser = await chromium.launch({ headless: !process.env.PW_HEADED });
  const ctx = await browser.newContext({
    viewport: { width: 1280, height: 720 },
    recordVideo: { dir: '/root/recordings/videos', size: { width: 1280, height: 720 } },
  });
  const page = await ctx.newPage();
  await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 30000 });
  const end = Date.now() + secs * 1000;
  while (Date.now() < end) {
    await page.mouse.wheel(0, 300); // slow scroll so the video shows the page
    await page.waitForTimeout(700);
  }
  const video = page.video();
  await ctx.close(); // finalizes the video file
  const slug = url.replace(/https?:\/\//, '').replace(/[^a-zA-Z0-9.-]+/g, '_').slice(0, 60);
  const dest = `/root/recordings/videos/${slug}-${Date.now()}.webm`;
  fs.renameSync(await video.path(), dest);
  console.log('recorded: ' + dest.replace('/root/recordings/', ''));
  await browser.close();
})().catch(e => { console.error(e.message); process.exit(1); });
