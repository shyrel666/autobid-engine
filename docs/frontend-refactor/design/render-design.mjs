// 只渲染本目录的静态样稿；不启动业务服务、不访问业务 API。
// 需要 Node.js 和可用的 playwright 或 playwright-core；可用环境变量指定模块及浏览器。
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { dirname, join } from 'node:path';
import { writeFile, readFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const folder = dirname(fileURLToPath(import.meta.url));
const cases = [
  ['01-project-list', 'view=projects', 1440, 900],
  ['02-execution-workspace', 'view=execution', 1440, 900],
  ['03-model-settings', 'view=models', 1440, 900],
  ['04-execution-expanded', 'view=execution&detail=expanded', 1440, 900],
  ['05-sidebar-collapsed', 'view=projects&sidebar=collapsed', 1440, 900],
  ['06-writing-workspace', 'view=writing&sidebar=collapsed', 1440, 900],
  ['07-execution-mobile', 'view=execution', 390, 844],
];
const browser = await chromium.launch({headless:true, ...(process.env.BROWSER_EXECUTABLE ? {executablePath:process.env.BROWSER_EXECUTABLE} : {})});
const manifest = {kind:'design-prototype', browser:browser.version(), platform:process.platform, deviceScaleFactor:2, colorScheme:'light', locale:'zh-CN', timezoneId:'Asia/Shanghai', font:'Segoe UI, Microsoft YaHei, sans-serif', captures:[]};
try {
  for (const [name, query, width, height] of cases) {
    const context = await browser.newContext({viewport:{width,height}, deviceScaleFactor:2, colorScheme:'light', locale:'zh-CN', timezoneId:'Asia/Shanghai', reducedMotion:'reduce'});
    const page = await context.newPage();
    const failures = [];
    page.on('pageerror', e => failures.push(e.message));
    await page.goto(`${pathToFileURL(join(folder,'reference-layout.html')).href}?${query}`);
    await page.evaluate(() => document.fonts.ready);
    const measurements = await page.evaluate(() => ({
      scrollWidth:document.documentElement.scrollWidth,
      viewportWidth:innerWidth,
      regions:[...document.querySelectorAll('[data-region]')].map(el=>{ const r=el.getBoundingClientRect(); return {name:el.dataset.region,x:r.x,y:r.y,width:r.width,height:r.height}; }),
      text:document.body.innerText,
    }));
    if (failures.length || measurements.scrollWidth > width) throw new Error(`${name}: ${JSON.stringify({failures,scrollWidth:measurements.scrollWidth,width})}`);
    if (!measurements.text.includes('辅助起草与检查') && width >= 768 && !query.includes('collapsed')) throw new Error(`${name}: missing product context`);
    const file = `${name}.png`;
    await page.screenshot({path:join(folder,file),fullPage:false,animations:'disabled'});
    const sha256 = createHash('sha256').update(await readFile(join(folder,file))).digest('hex');
    const {text,...geometry} = measurements;
    manifest.captures.push({file,query,viewport:{width,height},sha256,...geometry});
    console.log(`${file}: ${width}x${height} @2x; no page errors or horizontal overflow`);
    await context.close();
  }
  await writeFile(join(folder,'capture-manifest.json'),JSON.stringify(manifest,null,2)+'\n');
} finally {
  await browser.close();
}
