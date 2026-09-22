const { chromium } = require('playwright-core');
const fs = require('fs');
const path = require('path');

const prompt = process.argv[2];
const accountId = process.argv[3];
const mediaType = process.argv[4] || 'image'; // 'image' or 'video'
let profileDir = process.argv[5];
const chatUrl = process.argv[6] || '';

if (!prompt || !accountId) {
    console.error("Usage: node playwright_media.js <prompt> <accountId> [mediaType] [profileDir] [chatUrl]");
    process.exit(1);
}

if (!profileDir) {
    profileDir = path.join('data', 'profiles', accountId);
}

fs.mkdirSync(profileDir, { recursive: true });

// Clean up any stale Chrome lock files to prevent profile locking
const cleanLockFiles = () => {
    try {
        fs.unlinkSync(path.join(profileDir, 'SingletonLock'));
        fs.unlinkSync(path.join(profileDir, 'SingletonSocket'));
        fs.unlinkSync(path.join(profileDir, 'SingletonCookie'));
    } catch (e) {}
};
cleanLockFiles();

let globalContext = null;

const handleExitSignal = async (signal) => {
    console.error(`[Playwright] Received signal ${signal}, closing context...`);
    if (globalContext) {
        try { await globalContext.close(); } catch (e) {}
    }
    cleanLockFiles();
    process.exit(1);
};

process.on('SIGINT', () => handleExitSignal('SIGINT'));
process.on('SIGTERM', () => handleExitSignal('SIGTERM'));

function findBrowserExecutable() {
    const browserPaths = [
        '/usr/bin/google-chrome',
        '/opt/google/chrome/chrome',
        '/usr/bin/microsoft-edge',
        '/usr/bin/chromium',
        '/usr/bin/chromium-browser',
        '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
        'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
        'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe'
    ];
    for (const p of browserPaths) {
        if (fs.existsSync(p)) return p;
    }
    return undefined;
}

(async () => {
    let context;
    try {
        const sysBrowser = findBrowserExecutable();
        const launchOptions = {
            headless: true,
            executablePath: sysBrowser,
            args: [
                '--no-sandbox',
                '--disable-dev-shm-usage',
                '--disable-blink-features=AutomationControlled',
                '--window-size=1280,800'
            ],
            viewport: { width: 1280, height: 800 }
        };

        try {
            if (fs.existsSync('config.json')) {
                const configData = JSON.parse(fs.readFileSync('config.json', 'utf8'));
                if (configData.proxy) {
                    launchOptions.proxy = { server: configData.proxy };
                    console.log(`[Playwright] Using Proxy: ${configData.proxy}`);
                }
            }
        } catch (e) {}

        try {
            context = await chromium.launchPersistentContext(profileDir, launchOptions);
        } catch (err) {
            if (launchOptions.proxy) {
                console.warn(`[Playwright] Proxy ${launchOptions.proxy.server} failed (${err.message}). Retrying direct connection...`);
                delete launchOptions.proxy;
                context = await chromium.launchPersistentContext(profileDir, launchOptions);
            } else {
                throw err;
            }
        }
        globalContext = context;
        let activeCookieStr = '';

        if (fs.existsSync('data/cookies.json')) {
            try {
                const cookiesData = JSON.parse(fs.readFileSync('data/cookies.json', 'utf8'));
                const activeCookie = cookiesData.find(c => c.id === accountId);
                if (!activeCookie) {
                    console.error(`[Playwright] Account ${accountId} not found in cookies.json. Cannot proceed.`);
                    process.exit(1);
                }
                activeCookieStr = activeCookie.cookie || '';
                const pwCookies = [];
                activeCookieStr.split(';').forEach(c => {
                    const parts = c.trim().split('=');
                    const name = parts[0] ? parts[0].trim() : '';
                    const value = parts.slice(1).join('=').trim();
                    if (name && value) {
                        pwCookies.push({ name, value, domain: '.google.com', path: '/', secure: true });
                    }
                });
                await context.addCookies(pwCookies).catch(() => {});
            } catch (err) {}
        }

        const page = context.pages().length > 0 ? context.pages()[0] : await context.newPage();

        page.on('crash', () => {
            console.error("[Playwright] Page crashed!");
            process.exit(1);
        });

        const targetUrl = (chatUrl && chatUrl.startsWith('https://gemini.google.com/app')) ? chatUrl : 'https://gemini.google.com/app';
        console.log(`[Playwright] Navigating to Gemini target URL for account ${accountId}: ${targetUrl}...`);
        await page.goto(targetUrl, { waitUntil: 'domcontentloaded', timeout: 45000 });
        await page.waitForTimeout(1500);

        // Auto-dismiss any Google Gemini disclaimer/welcome dialogs
        await page.evaluate(() => {
            const buttons = Array.from(document.querySelectorAll('button, [role="button"]'));
            for (const b of buttons) {
                const txt = (b.innerText || '').toLowerCase();
                if (txt.includes('tôi hiểu') || txt.includes('tôi hiểu') || txt.includes('i understand') || txt.includes('got it') || txt.includes('đã hiểu') || txt.includes('accept') || txt.includes('agree')) {
                    b.click();
                }
            }
        }).catch(() => {});

        const boxSelector = 'rich-textarea div[contenteditable="true"], [contenteditable="true"]';
        let box = await page.$(boxSelector);

        if (!box) {
            try {
                await page.waitForSelector(boxSelector, { timeout: 10000 });
                box = await page.$(boxSelector);
            } catch (e) {}
        }

        if (!box) {
            console.error("[Playwright] Chat box element not found on Gemini page.");
            await context.close();
            process.exit(1);
        }

        const MSG_SELECTOR = 'model-response, message-content, [class*="model-response"], .response-container-content';

        // Record pre-existing media URLs on page before submitting prompt to prevent capturing old media
        const existingMediaUrls = await page.evaluate(({ mType }) => {
            const urls = [];
            if (mType === 'image') {
                document.querySelectorAll('img').forEach(img => { if (img.src) urls.push(img.src); });
            } else {
                document.querySelectorAll('a[href*=".mp4"], a[href*="contribution.usercontent.google.com"], a[href*="usercontent.google.com/download"]').forEach(a => { if (a.href) urls.push(a.href); });
                document.querySelectorAll('video').forEach(v => { const s = v.src || v.currentSrc; if (s) urls.push(s); });
                document.querySelectorAll('video source[src]').forEach(s => { if (s.src) urls.push(s.src); });
            }
            return Array.from(new Set(urls));
        }, { mType: mediaType });

        console.log(`[Playwright] Captured ${existingMediaUrls.length} pre-existing ${mediaType} URL(s) on page before prompt submission.`);

        // Record initial count of response message blocks before sending prompt
        const initialMsgCount = await page.evaluate((msgSel) => {
            return document.querySelectorAll(msgSel).length;
        }, MSG_SELECTOR);

        console.log(`[Playwright] Initial message block count on page: ${initialMsgCount}`);

        // 1. Enable Media Generation Mode if requested ('image' or 'video')
        if (mediaType === 'image' || mediaType === 'video') {
            const targetLabel = mediaType === 'image' ? 'Tạo hình ảnh' : 'Tạo video';
            const enLabel = mediaType === 'image' ? 'Create image' : 'Create video';
            const chipLabel = mediaType === 'image' ? 'Hình ảnh' : 'Video';
            const enChipLabel = mediaType === 'image' ? 'Image' : 'Video';

            try {
                // Check if the chip/pill is already active in prompt area
                const alreadyActive = await page.evaluate(({ vi, en }) => {
                    const buttons = Array.from(document.querySelectorAll('button, mat-chip, .chip, [role="button"]'));
                    return buttons.some(b => {
                        const aria = (b.getAttribute('aria-label') || '').toLowerCase();
                        const txt = (b.innerText || '').toLowerCase();
                        return aria.includes(vi.toLowerCase()) || aria.includes(en.toLowerCase()) ||
                               txt.includes(vi.toLowerCase()) || txt.includes(en.toLowerCase());
                    });
                }, { vi: `bỏ chọn ${chipLabel}`, en: `deselect ${enChipLabel}` });

                if (alreadyActive) {
                    console.log(`[Playwright] ${targetLabel} mode is already enabled in prompt box.`);
                } else {
                    console.log(`[Playwright] Activating '${targetLabel}' mode via tools (+) menu (aspect ratio: auto)...`);
                    const toolsBtn = await page.$('button[aria-label*="Nội dung tải lên và công cụ" i], button:has-text("+"), [aria-label*="Upload and tools" i]');
                    if (toolsBtn) {
                        await toolsBtn.click();
                        await page.waitForTimeout(1000);

                        // Click menu item for target media
                        const clicked = await page.evaluate(({ vi, en }) => {
                            const items = Array.from(document.querySelectorAll('[role="menuitem"], .mat-mdc-menu-item, button, .label'));
                            for (const item of items) {
                                const txt = (item.innerText || '').trim().toLowerCase();
                                const aria = (item.getAttribute('aria-label') || '').trim().toLowerCase();
                                if (txt.includes(vi.toLowerCase()) || txt.includes(en.toLowerCase()) ||
                                    aria.includes(vi.toLowerCase()) || aria.includes(en.toLowerCase())) {
                                    item.click();
                                    return true;
                                }
                            }
                            return false;
                        }, { vi: targetLabel, en: enLabel });

                        if (clicked) {
                            console.log(`[Playwright] Successfully activated '${targetLabel}' mode (aspect ratio: auto)!`);
                            await page.waitForTimeout(1500);
                        } else {
                            console.warn(`[Playwright] '${targetLabel}' item not found in tools menu, proceeding with text prompt.`);
                            await page.keyboard.press('Escape').catch(() => {});
                        }
                    } else {
                        console.warn(`[Playwright] Tools (+) button not found, proceeding with text prompt.`);
                    }
                }
            } catch (err) {
                console.warn(`[Playwright] Note: Could not toggle ${targetLabel} mode: ${err.message}`);
            }
        }

        console.log(`[Playwright] Entering prompt for ${mediaType} generation...`);
        const inputSelector = 'rich-textarea div[contenteditable="true"], [contenteditable="true"]';
        try {
            await page.waitForSelector(inputSelector, { state: 'visible', timeout: 10000 });
            await page.click(inputSelector);
            await page.keyboard.type(prompt);
            await page.waitForTimeout(500);
        } catch (e) {
            // Fallback via DOM evaluation if element re-hydrated
            await page.evaluate(({ sel, text }) => {
                const el = document.querySelector(sel);
                if (el) {
                    el.focus();
                    el.innerText = text;
                    el.dispatchEvent(new Event('input', { bubbles: true }));
                    el.dispatchEvent(new Event('change', { bubbles: true }));
                }
            }, { sel: inputSelector, text: prompt });
            await page.waitForTimeout(500);
        }
        const sendBtn = await page.$('button.send-button, button[aria-label*="Send"], button[aria-label*="Gửi"]');
        if (sendBtn) {
            console.log("[Playwright] Clicking send button...");
            await sendBtn.click();
        } else {
            console.log("[Playwright] Pressing Enter key...");
            await page.keyboard.press('Enter');
        }

        console.log(`[Playwright] Polling for ${mediaType} output...`);
        const maxPolls = mediaType === 'video' ? 160 : 30;

        for (let i = 0; i < maxPolls; i++) {
            await page.waitForTimeout(mediaType === 'video' ? 2500 : 1500);

            const result = await page.evaluate(({ mType, initMsgCnt, msgSel, baselineUrls }) => {
                const bodyText = (document.body.innerText || '').toLowerCase();
                const existingSet = new Set(baselineUrls || []);

                if (mType === 'image') {
                    if (bodyText.includes("can't create it right now") ||
                        bodyText.includes("image creation isn't available") ||
                        bodyText.includes("i can't generate images") || 
                        bodyText.includes("i cannot create images") ||
                        bodyText.includes("can't create images")) {
                        return { error: "Gemini image creation is not available for this account/location." };
                    }

                    const msgs = Array.from(document.querySelectorAll(msgSel));
                    const targetMsgs = msgs.length > initMsgCnt ? msgs.slice(initMsgCnt) : msgs;

                    const imgs = [];
                    targetMsgs.forEach(m => {
                        m.querySelectorAll('img').forEach(img => {
                            const src = img.src || '';
                            const isAvatar = src.includes('/a/') || src.includes('w64-h64') || src.includes('s64-') || src.includes('avatar') || src.includes('branding') || src.includes('profile');
                            const isGoogleImg = src.includes('googleusercontent.com') || src.includes('ggpht.com') || src.includes('generativelanguage');
                            const isBlob = src.startsWith('blob:https://gemini.google.com/');
                            const isLarge = img.naturalWidth > 150 || img.width > 150 || img.closest('message-content') !== null;
                            if ((isGoogleImg || isBlob) && !isAvatar && isLarge && !existingSet.has(src)) {
                                imgs.push(src);
                            }
                        });
                    });

                    if (imgs.length > 0) {
                        return { urls: Array.from(new Set(imgs)) };
                    }
                } else if (mType === 'video') {
                    if (bodyText.includes("reached your limit") ||
                        bodyText.includes("generation limit") ||
                        bodyText.includes("daily limit") ||
                        bodyText.includes("try again tomorrow") ||
                        bodyText.includes("try again later") ||
                        bodyText.includes("i can't generate videos") || 
                        bodyText.includes("i cannot create videos") ||
                        bodyText.includes("video generation isn't available") ||
                        bodyText.includes("can't generate videos right now")) {
                        return { error: "Gemini account video generation limit/quota reached." };
                    }

                    // Support all Gemini web UI custom element tag names for response messages
                    const msgs = Array.from(document.querySelectorAll(msgSel));
                    const targetMsgs = msgs.length > initMsgCnt ? msgs.slice(initMsgCnt) : (initMsgCnt === 0 ? msgs : []);

                    const foundUrls = [];
                    targetMsgs.forEach(m => {
                        m.querySelectorAll('a[href*=".mp4"], a[href*="contribution.usercontent.google.com"], a[href*="usercontent.google.com/download"]').forEach(a => { if (a.href) foundUrls.push(a.href); });
                        m.querySelectorAll('video').forEach(v => { const s = v.src || v.currentSrc; if (s) foundUrls.push(s); });
                        m.querySelectorAll('video source[src]').forEach(s => { if (s.src) foundUrls.push(s.src); });
                    });
                    // Also scan entire DOM in case video is rendered outside standard response block
                    document.querySelectorAll('video').forEach(v => { const s = v.src || v.currentSrc; if (s) foundUrls.push(s); });
                    document.querySelectorAll('a[href*=".mp4"], a[href*="contribution.usercontent.google.com"], a[href*="usercontent.google.com/download"]').forEach(a => { if (a.href) foundUrls.push(a.href); });

                    // Filter out any media URLs that existed before prompt submission
                    const newUrls = foundUrls.filter(u => u && !existingSet.has(u));

                    if (newUrls.length > 0) {
                        // Return the newest newly generated video link
                        return { urls: [newUrls[newUrls.length - 1]] };
                    }
                }

                return null;
            }, { mType: mediaType, initMsgCnt: initialMsgCount, msgSel: MSG_SELECTOR, baselineUrls: existingMediaUrls });

            if (result) {
                if (result.error) {
                    console.error(`[Playwright] Error: ${result.error}`);
                    await context.close();
                    process.exit(1);
                }

                if (result.urls && result.urls.length > 0) {
                    console.log("[Playwright] Found media URLs:", JSON.stringify(result.urls));
                    if (mediaType === 'image') {
                        console.log("[Playwright] Converting generated image(s) to Base64 in browser context...");
                        const b64List = [];
                        for (const u of result.urls) {
                            let b64Data = null;

                            // 1. First priority: Direct fetch with Google session cookie at full resolution (=s2048)
                            if (u.startsWith('http')) {
                                try {
                                    let targetUrl = u;
                                    if (targetUrl.includes('googleusercontent.com')) {
                                        if (/=s\d+/.test(targetUrl)) {
                                            targetUrl = targetUrl.replace(/=s\d+/, '=s2048');
                                        } else if (!targetUrl.includes('=s')) {
                                            targetUrl += '=s2048';
                                        }
                                    }
                                    console.log(`[Playwright] Downloading full-resolution image from: ${targetUrl.slice(0, 100)}...`);
                                    let curUrl = targetUrl;
                                    for (let hop = 0; hop < 5; hop++) {
                                        const res = await fetch(curUrl, {
                                            headers: {
                                                'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
                                                'Referer': 'https://gemini.google.com/',
                                                'Cookie': activeCookieStr || ''
                                            },
                                            redirect: 'manual'
                                        });
                                        if (res.status >= 300 && res.status < 400) {
                                            const nextLoc = res.headers.get('location');
                                            if (!nextLoc) break;
                                            curUrl = nextLoc;
                                            continue;
                                        }
                                        if (res.ok) {
                                            const mime = res.headers.get('content-type') || 'image/png';
                                            if (mime.startsWith('image/')) {
                                                const ab = await res.arrayBuffer();
                                                if (ab.byteLength > 1000) {
                                                    b64Data = `data:${mime};base64,${Buffer.from(ab).toString('base64')}`;
                                                    console.log(`[Playwright] Retrieved original high-res image via CDN fetch (${ab.byteLength} bytes)`);
                                                }
                                            }
                                        }
                                        break;
                                    }
                                } catch (e) {
                                    console.warn(`[Playwright] CDN fetch error: ${e.message}`);
                                }
                            }

                            // 2. Second priority: Full-resolution canvas extraction via page evaluation
                            if (!b64Data) {
                                b64Data = await page.evaluate(async (imgSrc) => {
                                    try {
                                        if (imgSrc.startsWith('blob:')) {
                                            return new Promise((resolve) => {
                                                const img = document.createElement('img');
                                                img.onload = () => {
                                                    try {
                                                        const canvas = document.createElement('canvas');
                                                        canvas.width = img.naturalWidth || 1024;
                                                        canvas.height = img.naturalHeight || 1024;
                                                        const ctx = canvas.getContext('2d');
                                                        ctx.drawImage(img, 0, 0);
                                                        resolve(canvas.toDataURL('image/png'));
                                                    } catch (e) { resolve(null); }
                                                };
                                                img.onerror = () => resolve(null);
                                                img.src = imgSrc;
                                            });
                                        }
                                        const resp = await fetch(imgSrc);
                                        if (!resp.ok) return null;
                                        const blob = await resp.blob();
                                        return new Promise((resolve, reject) => {
                                            const reader = new FileReader();
                                            reader.onloadend = () => resolve(reader.result);
                                            reader.onerror = reject;
                                            reader.readAsDataURL(blob);
                                        });
                                    } catch (e) {
                                        return null;
                                    }
                                }, u);
                            }

                            // 3. Fallback: Element screenshot if high-res fetch failed
                            if (!b64Data) {
                                try {
                                    const imgEl = await page.$(`img[src="${u}"]`);
                                    if (imgEl) {
                                        const buf = await imgEl.screenshot({ type: 'jpeg', quality: 95 });
                                        if (buf && buf.length > 500) {
                                            b64Data = `data:image/jpeg;base64,${buf.toString('base64')}`;
                                            console.log(`[Playwright] Captured element screenshot fallback (${buf.length} bytes)`);
                                        }
                                    }
                                } catch (e) {
                                    console.warn(`[Playwright] Element screenshot fallback error: ${e.message}`);
                                }
                            }

                            if (b64Data) {
                                b64List.push(b64Data);
                            } else {
                                b64List.push(u);
                            }
                        }
                        // Sync updated cookies back to backend
                        try {
                            const currentCookies = await context.cookies(['https://gemini.google.com', 'https://google.com']);
                            if (currentCookies && currentCookies.length > 0) {
                                const cookieString = currentCookies.map(c => `${c.name}=${c.value}`).join('; ');
                                const sapisid = currentCookies.find(c => c.name === 'SAPISID');
                                const psid = currentCookies.find(c => c.name === '__Secure-1PSID');
                                if (sapisid && psid) {
                                    console.log(`__COOKIES__=${cookieString}`);
                                }
                            }
                        } catch (e) {}

                        console.log(`__CHAT_URL__=${page.url()}`);
                        console.log(`__MEDIA_URL__=${b64List.join('|||')}`);
                        await context.close();
                        process.exit(0);
                    } else {
                        console.log("[Playwright] Processing generated video...");
                        const b64VideoList = [];
                        for (const u of result.urls) {
                            // 1. Direct blob to Base64 in browser context
                            if (u.startsWith('blob:')) {
                                try {
                                    const blobB64 = await page.evaluate(async (blobUrl) => {
                                        try {
                                            const res = await fetch(blobUrl);
                                            const blob = await res.blob();
                                            return new Promise((resolve) => {
                                                const reader = new FileReader();
                                                reader.onloadend = () => resolve(reader.result);
                                                reader.onerror = () => resolve(null);
                                                reader.readAsDataURL(blob);
                                            });
                                        } catch (e) {
                                            return null;
                                        }
                                    }, u);
                                    if (blobB64 && blobB64.startsWith('data:video/')) {
                                        b64VideoList.push(blobB64);
                                        console.log(`[Playwright] Converted video blob directly to Data URL (${blobB64.length} chars)`);
                                        continue;
                                    }
                                } catch (e) {
                                    console.warn(`[Playwright] Video blob conversion error: ${e.message}`);
                                }
                            }

                            // 2. Direct HTTP fetch with session cookies
                            if (u.startsWith('http')) {
                                try {
                                    let curUrl = u;
                                    for (let hop = 0; hop < 6; hop++) {
                                        const res = await fetch(curUrl, {
                                            headers: {
                                                'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
                                                'Referer': 'https://gemini.google.com/',
                                                'Cookie': activeCookieStr || ''
                                            },
                                            redirect: 'manual'
                                        });
                                        if (res.status >= 300 && res.status < 400) {
                                            const loc = res.headers.get('location');
                                            if (!loc) break;
                                            curUrl = loc;
                                            continue;
                                        }
                                        if (res.ok) {
                                            const ct = res.headers.get('content-type') || '';
                                            if (ct.includes('video') || ct.includes('octet-stream') || curUrl.includes('.mp4')) {
                                                const ab = await res.arrayBuffer();
                                                if (ab.byteLength > 50000) {
                                                    b64VideoList.push(`data:video/mp4;base64,${Buffer.from(ab).toString('base64')}`);
                                                    console.log(`[Playwright] Downloaded video via direct HTTP fetch (${ab.byteLength} bytes)`);
                                                    break;
                                                }
                                            }
                                        }
                                        break;
                                    }
                                    if (b64VideoList.length > 0) continue;
                                } catch (e) {
                                    console.warn(`[Playwright] Video HTTP fetch error: ${e.message}`);
                                }
                            }

                            // 3. Browser download event fallback
                            try {
                                const downloadPromise = page.waitForEvent('download', { timeout: 25000 }).catch(() => null);
                                await page.evaluate((url) => {
                                    const a = document.createElement('a');
                                    a.href = url;
                                    a.download = 'video.mp4';
                                    document.body.appendChild(a);
                                    a.click();
                                    document.body.removeChild(a);
                                }, u);
                                const download = await downloadPromise;
                                if (download) {
                                    const dlPath = await download.path();
                                    if (dlPath && fs.existsSync(dlPath)) {
                                        const buf = fs.readFileSync(dlPath);
                                        if (buf && buf.length > 50000 && !buf.toString('utf8', 0, 200).includes('<!doctype html>') && !buf.toString('utf8', 0, 200).includes('<html')) {
                                            b64VideoList.push(`data:video/mp4;base64,${buf.toString('base64')}`);
                                            console.log(`[Playwright] Video downloaded successfully via browser (${buf.length} bytes)`);
                                        } else {
                                            console.error("[Playwright] Downloaded video file is invalid HTML or too small:", buf.length);
                                        }
                                    }
                                }
                            } catch (e) {
                                console.error("[Playwright] Video download error:", e.message);
                            }
                        }

                        if (b64VideoList.length === 0) {
                            console.warn("[Playwright] Video download event not triggered, returning direct video URL");
                            b64VideoList.push(result.urls[0]);
                        }

                        // Sync updated cookies back to backend
                        try {
                            const currentCookies = await context.cookies(['https://gemini.google.com', 'https://google.com']);
                            if (currentCookies && currentCookies.length > 0) {
                                const cookieString = currentCookies.map(c => `${c.name}=${c.value}`).join('; ');
                                const sapisid = currentCookies.find(c => c.name === 'SAPISID');
                                const psid = currentCookies.find(c => c.name === '__Secure-1PSID');
                                if (sapisid && psid) {
                                    console.log(`__COOKIES__=${cookieString}`);
                                }
                            }
                        } catch (e) {}

                        console.log(`__CHAT_URL__=${page.url()}`);
                        console.log(`__MEDIA_URL__=${b64VideoList.join('|||')}`);
                        await context.close();
                        process.exit(0);
                    }
                }
            }
        }

        console.error(`[Playwright] Timeout waiting for ${mediaType} output after ${maxPolls} attempts.`);
        await context.close();
        process.exit(1);

    } catch (e) {
        console.error("[Playwright] Unhandled Exception:", e.message);
        if (context) await context.close();
        process.exit(1);
    }
})();
