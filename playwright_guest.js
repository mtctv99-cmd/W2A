const { chromium } = require('playwright-core');
const fs = require('fs');

const prompt = process.argv[2];
if (!prompt) {
    console.error("Usage: node playwright_guest.js <prompt>");
    process.exit(1);
}

function findBrowser() {
    const paths = [
        '/usr/bin/google-chrome',
        '/opt/google/chrome/chrome',
        '/usr/bin/chromium-browser',
        '/usr/bin/chromium'
    ];
    for (const p of paths) {
        if (fs.existsSync(p)) return p;
    }
    return 'google-chrome';
}

(async () => {
    let browser = null;
    try {
        browser = await chromium.launch({
            executablePath: findBrowser(),
            headless: true,
            args: [
                '--no-sandbox',
                '--disable-dev-shm-usage',
                '--disable-gpu',
                '--no-first-run',
                '--no-default-browser-check'
            ]
        });

        const context = await browser.newContext({
            userAgent: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36',
            locale: 'vi-VN'
        });

        const page = await context.newPage();
        await page.goto('https://gemini.google.com/app', { waitUntil: 'networkidle', timeout: 30000 });
        await page.waitForTimeout(1500);

        const inputEl = await page.$('div[contenteditable="true"], rich-textarea p, textarea');
        if (!inputEl) {
            throw new Error("Cannot find Gemini guest input box");
        }

        await inputEl.click();
        await page.keyboard.type(prompt, { delay: 10 });
        await page.waitForTimeout(500);

        const sendBtn = await page.$('button[aria-label*="Gửi"], button[aria-label*="Send"], button.send-button, [data-test-id="send-button"]');
        if (sendBtn) {
            await sendBtn.click();
        } else {
            await page.keyboard.press('Enter');
        }

        let lastText = "";
        let sameCount = 0;
        for (let i = 0; i < 40; i++) {
            await page.waitForTimeout(800);
            const responses = await page.$$('message-content, .model-response-text, .response-text');
            if (responses.length > 0) {
                const latest = responses[responses.length - 1];
                const text = (await latest.innerText()).trim();
                if (text && text === lastText) {
                    sameCount++;
                    if (sameCount >= 2) {
                        break;
                    }
                } else if (text) {
                    lastText = text;
                    sameCount = 0;
                }
            }
        }

        if (!lastText) {
            throw new Error("No response from Gemini guest model");
        }

        console.log(`__TEXT__=${lastText}`);
        await browser.close();
        process.exit(0);
    } catch (e) {
        console.error(`[Playwright-Guest Error] ${e.message}`);
        if (browser) {
            try { await browser.close(); } catch (_) {}
        }
        process.exit(1);
    }
})();
