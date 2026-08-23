import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { chromium } from 'playwright-core';

const [htmlPath, chromePath, scenario = 'exclusive'] = process.argv.slice(2);
if (!htmlPath || !chromePath) {
  throw new Error('usage: node test/dashboard_date_range.mjs <dashboard-html-path> <google-chrome-path> [exclusive|end-time|end-time-reset|reverse|los-angeles-dst]');
}
if (!['exclusive', 'end-time', 'end-time-reset', 'reverse', 'los-angeles-dst'].includes(scenario)) {
  throw new Error(`unknown dashboard date-range browser scenario: ${scenario}`);
}

const dashboardHTML = await readFile(htmlPath);
const resourceBase = '/v0/resource/plugins/calendar-browser-test';
const timezoneId = scenario === 'los-angeles-dst' ? 'America/Los_Angeles' : 'UTC';
const initialRange = scenario === 'los-angeles-dst'
  ? { start: '2026-08-23T07:00:00.000Z', end: '2026-08-24T07:00:00.000Z' }
  : { start: '2026-08-23T00:00:00.000Z', end: '2026-08-24T00:00:00.000Z' };
const emptyInitial = {
  generated_at: '2026-08-23T00:00:00.000Z',
  last_used: '0001-01-01T00:00:00.000Z',
  models: [],
  sources: [],
  bucket_seconds: 86400,
};

async function setTimePickerValue(page, boundary, values) {
  for (const [part, value] of Object.entries(values)) {
    await page.locator(`#${boundary}TimePicker [data-time-part="${part}"]`).evaluate((field, nextValue) => {
      field.value = nextValue;
      field.dispatchEvent(new Event('input', { bubbles: true }));
    }, value);
  }
}

const server = createServer((request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  const sendJSON = (value) => {
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(JSON.stringify(value));
  };

  if (url.pathname === `${resourceBase}/dashboard`) {
    response.writeHead(200, { 'content-type': 'text/html; charset=utf-8' });
    response.end(dashboardHTML);
    return;
  }
  if (url.pathname === `${resourceBase}/preferences`) {
    sendJSON({
      time_range_mode: 'custom',
      time_range_start: initialRange.start,
      time_range_end: initialRange.end,
    });
    return;
  }
  if (url.pathname === `${resourceBase}/stats/initial`) {
    sendJSON(emptyInitial);
    return;
  }
  if (url.pathname === `${resourceBase}/stats/trends`) {
    sendJSON({ model_series: [], bucket_seconds: 86400 });
    return;
  }
  if (url.pathname === `${resourceBase}/stats/groups` || url.pathname === `${resourceBase}/requests`) {
    sendJSON({ items: [], total: 0 });
    return;
  }
  if (url.pathname === `${resourceBase}/costs`) {
    sendJSON({ summary: { requests: 0, priced_requests: 0, unpriced_requests: 0 }, models: [], price_book_revision: 0 });
    return;
  }
  if (url.pathname === `${resourceBase}/prices`) {
    sendJSON({ prices: {}, revision: 0 });
    return;
  }
  sendJSON({});
});

await new Promise((resolve, reject) => {
  server.once('error', reject);
  server.listen(0, '127.0.0.1', resolve);
});

const address = server.address();
const dashboardURL = `http://127.0.0.1:${address.port}${resourceBase}/dashboard`;
const browser = await chromium.launch({ executablePath: chromePath, headless: true });

try {
  const context = await browser.newContext({ timezoneId });
  const page = await context.newPage();
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error));

  await page.clock.install({ time: new Date('2026-08-23T12:00:00.000Z') });
  await page.goto(dashboardURL, { waitUntil: 'domcontentloaded' });
  await page.waitForResponse((response) => new URL(response.url()).pathname === `${resourceBase}/stats/initial`);

  await page.locator('#rangeButton').click();
  if (scenario === 'reverse') {
    await page.locator('[data-date="2026-08-23"]').click();
    await page.locator('[data-date="2026-08-21"]').click();
  } else {
    await page.locator('[data-date="2026-08-21"]').click();
    await page.locator('[data-date="2026-08-23"]').click();
  }

  if (scenario === 'exclusive') {
    const selectedEnd = page.locator('[data-date="2026-08-23"].range-end');
    if (await selectedEnd.count() !== 1) {
      throw new Error('selecting 2026-08-21 through 2026-08-23 must mark 2026-08-23 as .range-end');
    }

    const confirmedResponse = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return url.pathname === `${resourceBase}/stats/initial`
        && url.searchParams.get('start') === '2026-08-21T00:00:00.000Z';
    });
    await page.locator('#confirmDateRange').click();
    const confirmed = new URL((await confirmedResponse).url());
    if (confirmed.searchParams.get('start') !== '2026-08-21T00:00:00.000Z') {
      throw new Error(`expected confirmed start=2026-08-21T00:00:00.000Z, got ${confirmed.searchParams.get('start')}`);
    }
    if (confirmed.searchParams.get('end') !== '2026-08-24T00:00:00.000Z') {
      throw new Error(`expected confirmed end=2026-08-24T00:00:00.000Z, got ${confirmed.searchParams.get('end')}`);
    }
  } else if (scenario === 'los-angeles-dst') {
    const selectedEnd = page.locator('[data-date="2026-08-23"].range-end');
    if (await selectedEnd.count() !== 1) {
      throw new Error('America/Los_Angeles selection must mark 2026-08-23 as .range-end');
    }

    const confirmedResponse = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return url.pathname === `${resourceBase}/stats/initial`
        && url.searchParams.get('start') === '2026-08-21T07:00:00.000Z';
    });
    await page.locator('#confirmDateRange').click();
    const confirmed = new URL((await confirmedResponse).url());
    if (confirmed.searchParams.get('start') !== '2026-08-21T07:00:00.000Z') {
      throw new Error(`expected America/Los_Angeles start=2026-08-21T07:00:00.000Z, got ${confirmed.searchParams.get('start')}`);
    }
    if (confirmed.searchParams.get('end') !== '2026-08-24T07:00:00.000Z') {
      throw new Error(`expected America/Los_Angeles end=2026-08-24T07:00:00.000Z, got ${confirmed.searchParams.get('end')}`);
    }
  } else if (scenario === 'end-time' || scenario === 'end-time-reset') {
    await page.locator('#endTimeButton').click();
    await setTimePickerValue(page, 'end', { hour: '12', minute: '00', second: '00' });

    if (scenario === 'end-time-reset') {
      await setTimePickerValue(page, 'end', { hour: '00', minute: '00', second: '00' });
    }

    const selectedEnd = page.locator('[data-date="2026-08-23"].range-end');
    if (await selectedEnd.count() !== 1) {
      throw new Error(scenario === 'end-time'
        ? 'setting end time to 12:00:00 must keep 2026-08-23 as .range-end'
        : 'resetting end time from 12:00:00 to 00:00:00 must keep 2026-08-23 as .range-end');
    }

    const confirmedResponse = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return url.pathname === `${resourceBase}/stats/initial`
        && url.searchParams.get('start') === '2026-08-21T00:00:00.000Z';
    });
    await page.locator('#confirmDateRange').click();
    const confirmed = new URL((await confirmedResponse).url());
    const expectedEnd = scenario === 'end-time' ? '2026-08-23T12:00:00.000Z' : '2026-08-24T00:00:00.000Z';
    if (confirmed.searchParams.get('end') !== expectedEnd) {
      throw new Error(`expected ${scenario} end=${expectedEnd}, got ${confirmed.searchParams.get('end')}`);
    }
  } else {
    const selectedStart = page.locator('[data-date="2026-08-21"].range-start');
    if (await selectedStart.count() !== 1) {
      throw new Error('reverse selection 2026-08-23 through 2026-08-21 must mark 2026-08-21 as .range-start');
    }
    const selectedEnd = page.locator('[data-date="2026-08-23"].range-end');
    if (await selectedEnd.count() !== 1) {
      throw new Error('reverse selection 2026-08-23 through 2026-08-21 must mark 2026-08-23 as .range-end');
    }

    const confirmedResponse = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return url.pathname === `${resourceBase}/stats/initial`
        && url.searchParams.get('start') === '2026-08-21T00:00:00.000Z';
    });
    await page.locator('#confirmDateRange').click();
    const confirmed = new URL((await confirmedResponse).url());
    if (confirmed.searchParams.get('end') !== '2026-08-24T00:00:00.000Z') {
      throw new Error(`expected reverse-selection end=2026-08-24T00:00:00.000Z, got ${confirmed.searchParams.get('end')}`);
    }
  }
  if (pageErrors.length) {
    throw pageErrors[0];
  }
} finally {
  await browser.close();
  await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
}
