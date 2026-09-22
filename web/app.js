document.addEventListener("DOMContentLoaded", () => {
    // Navigation and tab switching
    const navItems = document.querySelectorAll(".nav-item");
    const tabContents = document.querySelectorAll(".tab-content");

    navItems.forEach(item => {
        item.addEventListener("click", () => {
            const tabId = item.getAttribute("data-tab");
            
            navItems.forEach(i => i.classList.remove("active"));
            tabContents.forEach(c => c.classList.remove("active"));

            item.classList.add("active");
            document.getElementById(tabId).classList.add("active");

            // Fetch tab-specific data
            if (tabId === "logs") {
                startLogPolling();
            } else {
                stopLogPolling();
            }

            if (tabId === "cookies") {
                fetchCookies();
            }

            if (tabId === "apikeys") {
                fetchApiKeys();
            }

            if (tabId === "models") {
                fetchModels();
            }

            if (tabId === "copilot") {
                fetchCopilotStatus();
            }
        });
    });

    // Toast notifications
    const toast = document.getElementById("toast");
    function showToast(message, isSuccess = true) {
        toast.textContent = message;
        toast.style.background = isSuccess 
            ? "linear-gradient(135deg, #10b981 0%, #059669 100%)" 
            : "linear-gradient(135deg, #ef4444 0%, #dc2626 100%)";
        toast.classList.add("show");
        setTimeout(() => {
            toast.classList.remove("show");
        }, 3000);
    }

    // Polling helpers
    let statsInterval = null;
    let logsInterval = null;

    function startStatsPolling() {
        fetchStats();
        statsInterval = setInterval(fetchStats, 2000);
    }

    function startLogPolling() {
        fetchLogs();
        logsInterval = setInterval(fetchLogs, 2000);
    }

    function stopLogPolling() {
        if (logsInterval) {
            clearInterval(logsInterval);
            logsInterval = null;
        }
    }

    // 1. Fetch Overview Stats
    async function fetchStats() {
        try {
            const res = await fetch("/admin/api/stats");
            if (!res.ok) throw new Error("HTTP " + res.status);
            const data = await res.json();

            document.getElementById("stat-requests").textContent = data.total_requests;
            document.getElementById("stat-tokens").textContent = data.total_tokens;
            document.getElementById("stat-cookies").textContent = data.active_cookies;
            document.getElementById("stat-apikeys").textContent = data.api_keys_count;
            document.getElementById("stat-last-request").textContent = data.last_request_at || "N/A";

            const authModeBadge = document.getElementById("stat-auth-mode");
            if (authModeBadge) {
                if (data.require_auth) {
                    authModeBadge.textContent = "Protected (Key Required)";
                    authModeBadge.className = "badge badge-warn";
                } else {
                    authModeBadge.textContent = "Public (Open)";
                    authModeBadge.className = "badge badge-success";
                }
            }

            const browserBadge = document.getElementById("stat-browser-running");
            if (data.is_browser_running) {
                browserBadge.textContent = "Auto Login Active";
                browserBadge.className = "badge badge-success";
            } else {
                browserBadge.textContent = "Idle";
                browserBadge.className = "badge badge-info";
            }
        } catch (err) {
            console.error("Error fetching stats:", err);
        }
    }

    // Dynamic API URL and Copy Button
    const apiBaseUrlEl = document.getElementById("api-base-url");
    if (apiBaseUrlEl) {
        apiBaseUrlEl.textContent = window.location.origin + "/v1";
    }

    document.querySelectorAll(".guide-url").forEach(el => {
        el.textContent = window.location.origin + "/v1";
    });

    // Replace hardcoded localhost:8081 in code examples with dynamic origin
    document.querySelectorAll("pre, code").forEach(el => {
        el.innerHTML = el.innerHTML.replace(/http:\/\/localhost:8081\/v1/g, window.location.origin + "/v1");
    });

    const btnCopyApiUrl = document.getElementById("btn-copy-api-url");
    if (btnCopyApiUrl) {
        btnCopyApiUrl.addEventListener("click", () => {
            const url = window.location.origin + "/v1";
            navigator.clipboard.writeText(url).then(() => {
                showToast("Base API URL copied to clipboard!");
            }).catch(() => {
                showToast("Failed to copy URL", false);
            });
        });
    }

    // 2. Cookie Pool Management
    const btnAutoLogin = document.getElementById("btn-auto-login");
    if (btnAutoLogin) {
        btnAutoLogin.addEventListener("click", async () => {
            try {
                btnAutoLogin.disabled = true;
                btnAutoLogin.textContent = "Launching Chrome...";
                const res = await fetch("/admin/api/cookies/auto-login", { method: "POST" });
                const data = await res.json();
                if (data.success) {
                    showToast("Chrome opened! Please login to your Google account.");
                } else {
                    showToast(data.message || "Failed to start Chrome.", false);
                }
            } catch (err) {
                showToast("Failed to launch Chrome automation.", false);
            } finally {
                setTimeout(() => {
                    btnAutoLogin.disabled = false;
                    btnAutoLogin.innerHTML = `<svg class="icon-small" viewBox="0 0 24 24"><path d="M19 13h-6v6h-2v-6H5v-2h6V5h2v6h6v2z"/></svg> Auto Connect Browser (Login Google)`;
                }, 3000);
            }
        });
    }

    const btnRefreshAccounts = document.getElementById("btn-refresh-accounts");
    if (btnRefreshAccounts) {
        btnRefreshAccounts.addEventListener("click", async () => {
            try {
                btnRefreshAccounts.disabled = true;
                btnRefreshAccounts.textContent = "⏳ Đang làm mới Profile & Cookie...";
                const res = await fetch("/admin/api/cookies/refresh", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({})
                });
                const data = await res.json();
                showToast(data.message || "Đã kích hoạt làm mới toàn bộ profile qua Chrome headless.");

                // Poll status until refresh finishes (up to 40s)
                let pollCount = 0;
                const pollInterval = setInterval(async () => {
                    pollCount++;
                    try {
                        const sRes = await fetch("/admin/api/stats");
                        const sData = await sRes.json();
                        fetchCookies();
                        if (!sData.is_refreshing || pollCount >= 20) {
                            clearInterval(pollInterval);
                            btnRefreshAccounts.disabled = false;
                            btnRefreshAccounts.innerHTML = `<svg class="icon-small" viewBox="0 0 24 24"><path d="M12 4V1L8 5l4 4V6c3.31 0 6 2.69 6 6 0 1.01-.25 1.97-.7 2.8l1.46 1.46C19.54 15.03 20 13.57 20 12c0-4.42-3.58-8-8-8zm0 14c-3.31 0-6-2.69-6-6 0-1.01.25-1.97.7-2.8L5.24 7.74C4.46 8.97 4 10.43 4 12c0 4.42 3.58 8 8 8v3l4-4-4-4v3z"/></svg> Auto-Refresh Profiles & Cookies`;
                            showToast("Đã hoàn tất làm mới tất cả cookie và model!");
                        }
                    } catch (e) {
                        if (pollCount >= 15) clearInterval(pollInterval);
                    }
                }, 2000);
            } catch (err) {
                showToast("Lỗi kích hoạt làm mới cookie.", false);
                btnRefreshAccounts.disabled = false;
                btnRefreshAccounts.innerHTML = `<svg class="icon-small" viewBox="0 0 24 24"><path d="M12 4V1L8 5l4 4V6c3.31 0 6 2.69 6 6 0 1.01-.25 1.97-.7 2.8l1.46 1.46C19.54 15.03 20 13.57 20 12c0-4.42-3.58-8-8-8zm0 14c-3.31 0-6-2.69-6-6 0-1.01.25-1.97.7-2.8L5.24 7.74C4.46 8.97 4 10.43 4 12c0 4.42 3.58 8 8 8v3l4-4-4-4v3z"/></svg> Auto-Refresh Profiles & Cookies`;
            }
        });
    }

    async function fetchCookies() {
        try {
            const res = await fetch("/admin/api/cookies");
            const data = await res.json();
            const container = document.getElementById("accounts-container");
            container.innerHTML = "";

            if (!data.cookies || data.cookies.length === 0) {
                container.innerHTML = `<p class="empty-state">No accounts loaded. Click "Auto Connect Browser" to add one.</p>`;
                return;
            }

            data.cookies.forEach(acc => {
                const card = document.createElement("div");
                card.className = "pool-card";
                const isHealthy = acc.healthy !== false;
                let badgeHtml;
                if (acc.rotate_auth_error) {
                    badgeHtml = `<span class="badge" style="background: rgba(239, 68, 68, 0.2); color: #f87171; border: 1px solid rgba(239, 68, 68, 0.4);">⚠ Session Expired (cần re-login)</span>`;
                } else if (!isHealthy) {
                    badgeHtml = `<span class="badge badge-warn" style="background: rgba(245, 158, 11, 0.2); color: #fbbf24; border: 1px solid rgba(245, 158, 11, 0.4);">Cooldown (${acc.cooldown_until || "Rate-Limited"})</span>`;
                } else {
                    badgeHtml = `<span class="badge badge-success">Active</span>`;
                }

                let roleBadgeHtml = "";
                if (acc.pool_type === "session") {
                    roleBadgeHtml = `<span class="badge" style="background: rgba(139, 92, 246, 0.2); color: #a78bfa; border: 1px solid rgba(139, 92, 246, 0.4);">🎯 Session (40%)</span>`;
                } else {
                    roleBadgeHtml = `<span class="badge" style="background: rgba(59, 130, 246, 0.2); color: #60a5fa; border: 1px solid rgba(59, 130, 246, 0.4);">⚡ Worker (60%)</span>`;
                }

                card.innerHTML = `
                    <div class="pool-card-header" style="display: flex; justify-content: space-between; align-items: center; gap: 8px; flex-wrap: wrap;">
                        <div style="display: flex; align-items: center; gap: 8px; flex-wrap: wrap;">
                            <h3 style="margin: 0;">${acc.id}</h3>
                            ${roleBadgeHtml}
                            ${badgeHtml}
                        </div>
                        <div style="display: flex; align-items: center; gap: 6px;">
                            <label style="font-size: 11px; color: var(--text-muted);">Phân vai:</label>
                            <select class="role-selector" data-id="${acc.id}" style="background: #1e293b; color: #f8fafc; border: 1px solid #334155; border-radius: 4px; padding: 2px 6px; font-size: 11px; cursor: pointer;">
                                <option value="auto" ${acc.configured_role === 'auto' ? 'selected' : ''}>Auto (40/60)</option>
                                <option value="session" ${acc.configured_role === 'session' ? 'selected' : ''}>🎯 Ghim Session</option>
                                <option value="worker" ${acc.configured_role === 'worker' ? 'selected' : ''}>⚡ Worker Xoay Vòng</option>
                            </select>
                        </div>
                    </div>
                    <div class="pool-card-body" style="display: flex; flex-direction: column; gap: 8px;">
                        <p style="font-size: 13px; color: var(--text-muted); word-break: break-all; margin: 0;">
                            <strong>SAPISID:</strong> ${acc.sapisid ? acc.sapisid.slice(0, 15) + "..." : "None"}
                        </p>
                        <p style="font-size: 12px; color: var(--text-muted); word-break: break-all; margin: 0;">
                            <strong>Profile Dir:</strong> <code>${acc.profile_dir || ("data/profiles/" + acc.id)}</code>
                        </p>
                        ${acc.google_api_key ? `
                        <div style="font-size: 12px; background: rgba(16, 185, 129, 0.1); border: 1px solid rgba(16, 185, 129, 0.3); border-radius: 6px; padding: 6px 8px; display: flex; flex-direction: column; gap: 4px;">
                            <div style="display: flex; align-items: center; justify-content: space-between;">
                                <span style="color: #10b981; font-weight: 500;">Google API Key:</span>
                                <div style="display: flex; gap: 4px;">
                                    <button class="btn btn-secondary btn-toggle-view-key" data-id="${acc.id}" style="padding: 2px 8px; font-size: 10px;">👁️ Xem</button>
                                    <button class="btn btn-secondary btn-copy-google-key" data-key="${acc.google_api_key}" style="padding: 2px 8px; font-size: 10px;">Copy</button>
                                </div>
                            </div>
                            <code id="key-display-${acc.id}" data-full="${acc.google_api_key}" data-masked="${acc.google_api_key.slice(0, 8)}...${acc.google_api_key.slice(-4)}" style="color: #34d399; font-size: 11px; word-break: break-all;">${acc.google_api_key.slice(0, 8)}...${acc.google_api_key.slice(-4)}</code>
                        </div>
                        ` : ''}
                        ${acc.fail_count ? `<p style="font-size: 12px; color: #ef4444; margin: 0;"><strong>Fail Count:</strong> ${acc.fail_count}</p>` : ''}
                        <div style="display: flex; gap: 8px; margin-top: 4px; flex-wrap: wrap;">
                            <button class="btn btn-primary btn-refresh-single" data-id="${acc.id}" style="padding: 6px 12px; font-size: 12px;">⚡ Làm mới Cookie (CDP)</button>
                            <button class="btn btn-secondary btn-relogin-cookie" data-id="${acc.id}" style="padding: 6px 12px; font-size: 12px;">Mở Profile (Trình duyệt)</button>
                            <button class="btn btn-studio btn-aistudio-cookie" data-id="${acc.id}" style="padding: 6px 12px; font-size: 12px;">${acc.google_api_key ? '🔑 Mở AI Studio' : '🔑 Mở AI Studio & Auto'}</button>
                            <button class="btn btn-secondary btn-set-google-key" data-id="${acc.id}" style="padding: 6px 12px; font-size: 12px;">✏️ ${acc.google_api_key ? 'Đổi Key' : 'Dán Key'}</button>
                            <button class="btn btn-danger btn-delete-cookie" data-id="${acc.id}" style="padding: 6px 12px; font-size: 12px;">Delete</button>
                        </div>
                    </div>
                `;
                container.appendChild(card);
            });

            // Attach role selector handlers
            document.querySelectorAll(".role-selector").forEach(sel => {
                sel.addEventListener("change", async () => {
                    const id = sel.getAttribute("data-id");
                    const newRole = sel.value;
                    try {
                        const res = await fetch("/admin/api/cookies/pool-type", {
                            method: "POST",
                            headers: { "Content-Type": "application/json" },
                            body: JSON.stringify({ id, pool_type: newRole })
                        });
                        if (res.ok) {
                            fetchCookies();
                        }
                    } catch (err) {
                        alert("Lỗi cập nhật vai trò: " + err.message);
                    }
                });
            });

            // Attach single account CDP refresh handlers
            document.querySelectorAll(".btn-refresh-single").forEach(btn => {
                btn.addEventListener("click", async () => {
                    const id = btn.getAttribute("data-id");
                    try {
                        btn.disabled = true;
                        btn.textContent = "⏳ Đang quét...";
                        const res = await fetch("/admin/api/cookies/refresh", {
                            method: "POST",
                            headers: { "Content-Type": "application/json" },
                            body: JSON.stringify({ id })
                        });
                        const data = await res.json();
                        showToast(data.message || `Đang làm mới cookie cho ${id}...`);
                        setTimeout(() => {
                            fetchCookies();
                            fetchStats();
                            btn.disabled = false;
                            btn.textContent = "⚡ Làm mới Cookie (CDP)";
                        }, 5000);
                    } catch (err) {
                        showToast("Lỗi làm mới cookie.", false);
                        btn.disabled = false;
                        btn.textContent = "⚡ Làm mới Cookie (CDP)";
                    }
                });
            });

            // Attach relogin handlers
            document.querySelectorAll(".btn-relogin-cookie").forEach(btn => {
                btn.addEventListener("click", async () => {
                    const id = btn.getAttribute("data-id");
                    try {
                        btn.disabled = true;
                        const res = await fetch("/admin/api/cookies/auto-login", {
                            method: "POST",
                            headers: { "Content-Type": "application/json" },
                            body: JSON.stringify({ id })
                        });
                        const data = await res.json();
                        if (data.success) {
                            showToast(`Đã mở trình duyệt cho ${id}. Hãy thao tác xong rồi đóng tab/trình duyệt.`, true);
                        } else {
                            showToast(data.message || "Lỗi mở trình duyệt.", false);
                        }
                        setTimeout(() => { btn.disabled = false; }, 3000);
                    } catch (err) {
                        showToast("Lỗi kết nối máy chủ.", false);
                        btn.disabled = false;
                    }
                });
            });

            // Attach AI Studio handlers
            document.querySelectorAll(".btn-aistudio-cookie").forEach(btn => {
                btn.addEventListener("click", async () => {
                    const id = btn.getAttribute("data-id");
                    try {
                        btn.disabled = true;
                        const res = await fetch("/admin/api/cookies/auto-login", {
                            method: "POST",
                            headers: { "Content-Type": "application/json" },
                            body: JSON.stringify({ id, url: "https://aistudio.google.com/app/apikey" })
                        });
                        const data = await res.json();
                        if (data.success) {
                            showToast(`Đang mở AI Studio cho ${id}. Hãy đăng nhập nếu được hỏi, hệ thống sẽ tự động bắt API Key khi vào trang.`, true);
                        } else {
                            showToast(data.message || "Lỗi mở AI Studio.", false);
                        }
                        setTimeout(() => { btn.disabled = false; }, 3000);
                    } catch (err) {
                        showToast("Lỗi kết nối máy chủ.", false);
                        btn.disabled = false;
                    }
                });
            });

            // Attach Toggle View Google API Key handlers
            document.querySelectorAll(".btn-toggle-view-key").forEach(btn => {
                btn.addEventListener("click", () => {
                    const id = btn.getAttribute("data-id");
                    const codeEl = document.getElementById(`key-display-${id}`);
                    if (codeEl) {
                        const isMasked = codeEl.textContent === codeEl.getAttribute("data-masked");
                        if (isMasked) {
                            codeEl.textContent = codeEl.getAttribute("data-full");
                            btn.textContent = "🙈 Ẩn";
                        } else {
                            codeEl.textContent = codeEl.getAttribute("data-masked");
                            btn.textContent = "👁️ Xem";
                        }
                    }
                });
            });

            // Attach Manual Set Google API Key handlers
            document.querySelectorAll(".btn-set-google-key").forEach(btn => {
                btn.addEventListener("click", async () => {
                    const id = btn.getAttribute("data-id");
                    const inputKey = prompt(`Nhập Google API Key chính thức (AQ... hoặc AIzaSy...) cho ${id}:`);
                    if (inputKey === null) return;
                    const cleanKey = inputKey.trim();
                    try {
                        const res = await fetch("/admin/api/cookies/google-key", {
                            method: "POST",
                            headers: { "Content-Type": "application/json" },
                            body: JSON.stringify({ id, key: cleanKey })
                        });
                        const data = await res.json();
                        if (data.success) {
                            showToast(`Đã lưu Google API Key cho ${id}!`, true);
                            fetchCookies();
                        } else {
                            showToast(data.message || "Lỗi lưu key.", false);
                        }
                    } catch (err) {
                        showToast("Lỗi kết nối máy chủ.", false);
                    }
                });
            });

            // Attach Copy Google API Key handlers
            document.querySelectorAll(".btn-copy-google-key").forEach(btn => {
                btn.addEventListener("click", () => {
                    const key = btn.getAttribute("data-key");
                    if (key) {
                        navigator.clipboard.writeText(key).then(() => {
                            showToast("Đã copy Google API Key!", true);
                        }).catch(() => {
                            showToast("Không thể copy tự động.", false);
                        });
                    }
                });
            });

            // Attach delete handlers for cookies
            document.querySelectorAll(".btn-delete-cookie").forEach(btn => {
                btn.addEventListener("click", async () => {
                    const id = btn.getAttribute("data-id");
                    if (confirm(`Are you sure you want to delete ${id}?`)) {
                        try {
                            const res = await fetch("/admin/api/cookies/delete", {
                                method: "POST",
                                headers: { "Content-Type": "application/json" },
                                body: JSON.stringify({ id })
                            });
                            const result = await res.json();
                            if (result.success) {
                                showToast(`Deleted ${id}`);
                                fetchCookies();
                                fetchStats();
                            } else {
                                showToast(result.message || "Failed to delete account", false);
                            }
                        } catch (err) {
                            showToast("Failed to delete account.", false);
                        }
                    }
                });
            });
        } catch (err) {
            console.error("Error fetching cookies:", err);
        }
    }

    // 3. API Keys Management
    const btnAddKey = document.getElementById("btn-add-key");
    const inputNewKey = document.getElementById("input-new-key");

    if (btnAddKey && inputNewKey) {
    btnAddKey.addEventListener("click", async () => {
        const key = inputNewKey.value.trim();
        try {
            const res = await fetch("/admin/api/apikeys/add", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ key })
            });
            const data = await res.json();
            if (data.success) {
                showToast(`API Key added successfully! ${data.key ? "Key: " + data.key : ""}`);
                inputNewKey.value = "";
                fetchApiKeys();
                fetchStats();
            } else {
                showToast(data.message || "Failed to add API key", false);
            }
        } catch (err) {
            showToast("Network error occurred.", false);
        }
    });
    }

    async function fetchApiKeys() {
        try {
            const res = await fetch("/admin/api/apikeys");
            const data = await res.json();

            // Update Auth Mode UI
            const authBadge = document.getElementById("auth-mode-badge");
            const authDesc = document.getElementById("auth-mode-desc");
            const btnToggleAuth = document.getElementById("btn-toggle-auth");

            if (authBadge && authDesc && btnToggleAuth) {
                if (data.require_auth) {
                    authBadge.textContent = "Protected (API Key Required)";
                    authBadge.className = "badge badge-warn";
                    authDesc.textContent = "Clients must provide a valid Authorization: Bearer <key> header.";
                    btnToggleAuth.textContent = "Switch to Public Mode (Disable Auth)";
                    btnToggleAuth.className = "btn btn-secondary";
                } else {
                    authBadge.textContent = "Public (No Key Required)";
                    authBadge.className = "badge badge-success";
                    authDesc.textContent = "Anyone can make API calls without authentication.";
                    btnToggleAuth.textContent = "Switch to Protected Mode (Enable Auth)";
                    btnToggleAuth.className = "btn btn-primary";
                }
            }

            const tbody = document.getElementById("apikeys-table-body");
            tbody.innerHTML = "";

            if (!data.keys || data.keys.length === 0) {
                tbody.innerHTML = `<tr><td colspan="2" class="empty-state">No active keys. Anyone can request the API.</td></tr>`;
                return;
            }

            data.keys.forEach(key => {
                const tr = document.createElement("tr");
                const hiddenKey = key.slice(0, 8) + "****************";
                tr.innerHTML = `
                    <td><code>${hiddenKey}</code></td>
                    <td>
                        <button class="btn btn-danger btn-delete-key" data-key="${key}">Delete</button>
                    </td>
                `;
                tbody.appendChild(tr);
            });

            // Attach delete handlers
            document.querySelectorAll(".btn-delete-key").forEach(btn => {
                btn.addEventListener("click", async () => {
                    const keyToDelete = btn.getAttribute("data-key");
                    if (confirm("Are you sure you want to delete this API Key?")) {
                        try {
                            const res = await fetch("/admin/api/apikeys/delete", {
                                method: "POST",
                                headers: { "Content-Type": "application/json" },
                                body: JSON.stringify({ key: keyToDelete })
                            });
                            const result = await res.json();
                            if (result.success) {
                                showToast("API Key deleted.");
                                fetchApiKeys();
                                fetchStats();
                            }
                        } catch (err) {
                            showToast("Failed to delete key", false);
                        }
                    }
                });
            });
        } catch (err) {
            console.error("Error fetching keys:", err);
        }
    }

    const btnToggleAuth = document.getElementById("btn-toggle-auth");
    if (btnToggleAuth) {
        btnToggleAuth.addEventListener("click", async () => {
            try {
                btnToggleAuth.disabled = true;
                const res = await fetch("/admin/api/auth/toggle", { method: "POST" });
                const data = await res.json();
                if (data.success) {
                    showToast(data.require_auth ? "Switched to Protected Mode (API Key Required)!" : "Switched to Public Mode (No Key Required)!");
                    fetchApiKeys();
                    fetchStats();
                } else {
                    showToast(data.message || "Failed to toggle auth mode", false);
                }
            } catch (err) {
                showToast("Network error toggling auth mode", false);
            } finally {
                btnToggleAuth.disabled = false;
            }
        });
    }

    // 4. Models List with Live Search & Category Filtering
    const btnSyncModels = document.getElementById("btn-sync-models");
    let allFetchedModels = {};
    let currentModelFilter = "all";
    let currentModelSearch = "";

    if (btnSyncModels) {
        btnSyncModels.addEventListener("click", async () => {
            try {
                btnSyncModels.disabled = true;
                btnSyncModels.textContent = "Syncing with Google...";
                const res = await fetch("/admin/api/models/sync", { method: "POST" });
                const data = await res.json();
                if (data.success) {
                    showToast("Triggered live model registry sync!");
                    setTimeout(() => {
                        fetchModels();
                    }, 4000);
                } else {
                    showToast(data.message || "Model sync failed.", false);
                }
            } catch (err) {
                showToast("Error triggering model sync.", false);
            } finally {
                setTimeout(() => {
                    btnSyncModels.disabled = false;
                    btnSyncModels.innerHTML = `<svg class="icon-small" viewBox="0 0 24 24"><path d="M12 4V1L8 5l4 4V6c3.31 0 6 2.69 6 6 0 1.01-.25 1.97-.7 2.8l1.46 1.46C19.54 15.03 20 13.57 20 12c0-4.42-3.58-8-8-8zm0 14c-3.31 0-6-2.69-6-6 0-1.01.25-1.97.7-2.8L5.24 7.74C4.46 8.97 4 10.43 4 12c0 4.42 3.58 8 8 8v3l4-4-4-4v3z"/></svg> Sync Live Models`;
                }, 3000);
            }
        });
    }

    // Filter and search bindings
    const modelFilters = document.getElementById("model-category-filters");
    if (modelFilters) {
        modelFilters.querySelectorAll(".filter-btn").forEach(btn => {
            btn.addEventListener("click", () => {
                modelFilters.querySelectorAll(".filter-btn").forEach(b => b.classList.remove("active"));
                btn.classList.add("active");
                currentModelFilter = btn.getAttribute("data-filter") || "all";
                renderFilteredModels();
            });
        });
    }

    const modelSearchInput = document.getElementById("model-search");
    if (modelSearchInput) {
        modelSearchInput.addEventListener("input", (e) => {
            currentModelSearch = (e.target.value || "").toLowerCase().trim();
            renderFilteredModels();
        });
    }

    function renderFilteredModels() {
        const container = document.getElementById("models-container");
        if (!container) return;
        container.innerHTML = "";

        const entries = Object.entries(allFetchedModels);
        if (entries.length === 0) {
            container.innerHTML = '<p class="empty-state">No models loaded. Click "Sync Live Models" to refresh.</p>';
            return;
        }

        const filtered = entries.filter(([name, cfg]) => {
            // Category filter
            if (currentModelFilter !== "all") {
                const cat = cfg.category || (name.startsWith("copilot") || name.startsWith("gpt-") ? "copilot" : (name.includes("imagen") || name.includes("veo") ? "media" : "gemini"));
                if (currentModelFilter === "gemini" && cat !== "gemini") return false;
                if (currentModelFilter === "media" && cat !== "media") return false;
                if (currentModelFilter === "copilot" && cat !== "copilot") return false;
            }
            // Search filter
            if (currentModelSearch) {
                const matchName = name.toLowerCase().includes(currentModelSearch);
                const matchDesc = (cfg.desc || "").toLowerCase().includes(currentModelSearch);
                if (!matchName && !matchDesc) return false;
            }
            return true;
        });

        if (filtered.length === 0) {
            container.innerHTML = `<p class="empty-state" style="grid-column: 1/-1;">No models matching filter "${currentModelFilter}" and query "${currentModelSearch}".</p>`;
            return;
        }

        filtered.forEach(([name, cfg]) => {
            const card = document.createElement("div");
            card.className = "model-card";

            const cat = cfg.category || (name.startsWith("copilot") || name.startsWith("gpt-") ? "copilot" : (name.includes("imagen") || name.includes("veo") ? "media" : "gemini"));
            let tagBadge = "";
            if (cat === "copilot") {
                tagBadge = `<span class="badge" style="background: rgba(99, 102, 241, 0.2); color: #a5b4fc; border: 1px solid rgba(99, 102, 241, 0.4);">🤖 Copilot / GPT-5</span>`;
            } else if (cat === "media") {
                tagBadge = `<span class="badge" style="background: rgba(236, 72, 153, 0.2); color: #f472b6; border: 1px solid rgba(236, 72, 153, 0.4);">🎨 Media Generation</span>`;
            } else {
                tagBadge = `<span class="badge badge-info">⚡ Gemini Web</span>`;
            }

            let tokenBadge = "";
            if (cfg.input_limit && cfg.input_limit > 1000) {
                const limitK = cfg.input_limit >= 1000000 ? `${cfg.input_limit / 1000000}M` : `${cfg.input_limit / 1000}K`;
                tokenBadge = `<span class="badge" style="background: rgba(255, 255, 255, 0.05); color: #cbd5e1;">Context: ${limitK} Tokens</span>`;
            }

            card.innerHTML = `
                <div style="display: flex; justify-content: space-between; align-items: flex-start; gap: 8px;">
                    <h3 style="margin: 0; word-break: break-all; font-size: 15px;">${name}</h3>
                    <button class="btn btn-secondary btn-copy-model-name" data-name="${name}" style="padding: 2px 8px; font-size: 10px; flex-shrink: 0;">Copy</button>
                </div>
                <p style="margin: 8px 0; font-size: 12px; color: var(--text-muted); line-height: 1.4;">${cfg.desc || "Official Gemini model"}</p>
                <div class="model-meta" style="display: flex; gap: 6px; flex-wrap: wrap; margin-top: auto;">
                    ${tagBadge}
                    ${tokenBadge}
                </div>
            `;
            container.appendChild(card);
        });

        // Attach copy model name
        container.querySelectorAll(".btn-copy-model-name").forEach(btn => {
            btn.addEventListener("click", () => {
                const n = btn.getAttribute("data-name");
                navigator.clipboard.writeText(n).then(() => {
                    showToast(`Copied model ID: ${n}!`, true);
                });
            });
        });
    }

    async function fetchModels() {
        try {
            const res = await fetch("/admin/api/models");
            const data = await res.json();
            if (data && data.models && typeof data.models === 'object') {
                allFetchedModels = data.models;
                renderFilteredModels();
            }
        } catch (err) {
            console.error("Error fetching models:", err);
        }
    }

    // 5. Live Logs Terminal
    const logsTerminal = document.getElementById("logs-terminal");
    const btnClearLogs = document.getElementById("btn-clear-logs");

    if (!logsTerminal || !btnClearLogs) {
        console.warn("Logs terminal elements missing — skipping event binding");
    } else {
    btnClearLogs.addEventListener("click", async () => {
        logsTerminal.textContent = "Clearing logs...";
        try {
            await fetch("/admin/api/logs/clear", { method: "POST" });
            logsTerminal.textContent = "Logs cleared.";
            showToast("Server log file cleared.");
        } catch (err) {
            logsTerminal.textContent = "";
        }
    });
    }

    async function fetchLogs() {
        try {
            const res = await fetch("/admin/api/logs");
            const data = await res.json();
            logsTerminal.textContent = data.logs || "No logs available.";
            logsTerminal.scrollTop = logsTerminal.scrollHeight;
        } catch (err) {
            logsTerminal.textContent = "Connection to server logs lost...";
        }
    }

    async function fetchCopilotStatus() {
        try {
            const res = await fetch("/admin/api/copilot/status");
            const data = await res.json();
            const userEl = document.getElementById("copilot-user");
            const timerEl = document.getElementById("copilot-timer");
            const badgeEl = document.getElementById("copilot-status-badge");

            if (userEl) userEl.innerText = data.user || "Chưa cấu hình";
            if (timerEl) timerEl.innerText = data.remaining_formatted || "Hết hạn";
            if (badgeEl) {
                if (data.valid) {
                    badgeEl.className = "badge badge-success";
                    badgeEl.innerText = "ACTIVE";
                } else {
                    badgeEl.className = "badge badge-danger";
                    badgeEl.innerText = "EXPIRED";
                }
            }

            const btnStopLogin = document.getElementById("btn-copilot-stop-login");
            if (btnStopLogin) {
                btnStopLogin.style.display = data.is_logging_in ? "inline-flex" : "none";
            }

            // Populate account selector dropdown for manual token paste
            const accSelect = document.getElementById("select-copilot-token-account");
            if (accSelect && data.accounts) {
                const currentVal = accSelect.value;
                accSelect.innerHTML = `<option value="">Tự động khớp / Mặc định</option>`;
                data.accounts.forEach(acc => {
                    const opt = document.createElement("option");
                    opt.value = acc.id;
                    opt.textContent = `${acc.id} (${acc.user || 'Chưa đăng nhập'})`;
                    if (opt.value === currentVal) opt.selected = true;
                    accSelect.appendChild(opt);
                });
            }

            const accountsContainer = document.getElementById("copilot-accounts-container");
            if (accountsContainer) {
                accountsContainer.innerHTML = "";
                if (!data.accounts || data.accounts.length === 0) {
                    accountsContainer.innerHTML = `<p class="empty-state">Chưa có tài khoản Copilot nào. Bấm "Thêm tài khoản mới" hoặc dán token URL để bắt đầu.</p>`;
                } else {
                    data.accounts.forEach(acc => {
                        const card = document.createElement("div");
                        card.className = "pool-card";
                        
                        let badgeHtml = "";
                        if (acc.active) {
                            badgeHtml += `<span class="badge badge-info" style="margin-right: 4px;">Đang dùng</span> `;
                        }

                        if (!acc.healthy && acc.cooldown_until) {
                            badgeHtml += `<span class="badge badge-warning">Cooldown (${acc.cooldown_until})</span>`;
                        } else if (acc.remaining_seconds > 0) {
                            badgeHtml += `<span class="badge badge-success">Active</span>`;
                        } else {
                            badgeHtml += `<span class="badge badge-danger">Expired</span>`;
                        }

                        let roleBadgeHtml = "";
                        if (acc.pool_type === "session") {
                            roleBadgeHtml = `<span class="badge" style="background: rgba(139, 92, 246, 0.2); color: #a78bfa; border: 1px solid rgba(139, 92, 246, 0.4);">🎯 Session (40%)</span>`;
                        } else {
                            roleBadgeHtml = `<span class="badge" style="background: rgba(59, 130, 246, 0.2); color: #60a5fa; border: 1px solid rgba(59, 130, 246, 0.4);">⚡ Worker (60%)</span>`;
                        }

                        const pDir = acc.profile_dir || `data/copilot/profiles/${acc.id}`;

                        card.innerHTML = `
                            <div class="pool-card-header" style="display: flex; justify-content: space-between; align-items: center; gap: 8px; flex-wrap: wrap;">
                                <div style="display: flex; align-items: center; gap: 8px; flex-wrap: wrap;">
                                    <h3 style="font-size: 14px; margin: 0; font-weight: 700; color: #60a5fa;">${acc.id}</h3>
                                    <span style="font-size: 13px; color: #cbd5e1; word-break: break-all;">${acc.user}</span>
                                    ${roleBadgeHtml}
                                    ${badgeHtml}
                                </div>
                                <div style="display: flex; gap: 6px; align-items: center; flex-shrink: 0; flex-wrap: wrap;">
                                    <select class="copilot-role-selector" data-id="${acc.id}" style="background: #1e293b; color: #f8fafc; border: 1px solid #334155; border-radius: 4px; padding: 3px 6px; font-size: 11px; cursor: pointer;">
                                        <option value="auto" ${acc.configured_role === 'auto' ? 'selected' : ''}>Auto (40/60)</option>
                                        <option value="session" ${acc.configured_role === 'session' ? 'selected' : ''}>🎯 Ghim Session</option>
                                        <option value="worker" ${acc.configured_role === 'worker' ? 'selected' : ''}>⚡ Worker Xoay Vòng</option>
                                    </select>
                                    <button class="btn btn-primary btn-relogin-copilot" data-id="${acc.id}" style="padding: 4px 10px; font-size: 11px;">Mở Đăng nhập</button>
                                    <button class="btn btn-secondary btn-refresh-copilot-single" data-id="${acc.id}" style="padding: 4px 10px; font-size: 11px;">⚡ Làm mới (CDP)</button>
                                    <button class="btn btn-danger btn-delete-copilot-acc" data-id="${acc.id}" style="padding: 4px 10px; font-size: 11px;">Xóa</button>
                                </div>
                            </div>
                            <div style="font-size: 12px; color: var(--text-muted); margin-top: 10px; display: flex; flex-direction: column; gap: 4px;">
                                <div>Thời hạn: <b style="color: ${acc.remaining_seconds > 0 ? '#34d399' : '#f87171'};">${acc.remaining_formatted}</b></div>
                                <div style="font-family: monospace; font-size: 11px; color: #94a3b8; word-break: break-all;">Profile: ${pDir}</div>
                                ${acc.fail_count ? `<div style="color: #ef4444;">Số lần lỗi: <b>${acc.fail_count}</b></div>` : ''}
                            </div>
                        `;
                        accountsContainer.appendChild(card);
                    });

                    // Bind copilot role selector handlers
                    accountsContainer.querySelectorAll(".copilot-role-selector").forEach(sel => {
                        sel.addEventListener("change", async () => {
                            const id = sel.getAttribute("data-id");
                            const newRole = sel.value;
                            try {
                                const res = await fetch("/admin/api/copilot/pool-type", {
                                    method: "POST",
                                    headers: { "Content-Type": "application/json" },
                                    body: JSON.stringify({ id, pool_type: newRole })
                                });
                                if (res.ok) {
                                    fetchCopilotStatus();
                                }
                            } catch (err) {
                                alert("Lỗi cập nhật vai trò Copilot: " + err.message);
                            }
                        });
                    });

                    // Bind per-account re-login handler
                    accountsContainer.querySelectorAll(".btn-relogin-copilot").forEach(btn => {
                        btn.addEventListener("click", async () => {
                            const id = btn.getAttribute("data-id");
                            btn.disabled = true;
                            btn.textContent = "Đang mở...";
                            try {
                                const res = await fetch("/admin/api/copilot/auto-login", {
                                    method: "POST",
                                    headers: { "Content-Type": "application/json" },
                                    body: JSON.stringify({ id, force: false })
                                });
                                const data = await res.json();
                                if (data.success) {
                                    showToast(data.message || `Đã mở Chrome đăng nhập cho ${id} trên màn hình!`, true);
                                } else {
                                    if (confirm(data.message + "\n\nBạn có muốn đóng Chrome cũ và mở lại không?")) {
                                        const forceRes = await fetch("/admin/api/copilot/auto-login", {
                                            method: "POST",
                                            headers: { "Content-Type": "application/json" },
                                            body: JSON.stringify({ id, force: true })
                                        });
                                        const forceData = await forceRes.json();
                                        showToast(forceData.message || `Đã mở lại Chrome cho ${id}!`, forceData.success);
                                    }
                                }
                                fetchCopilotStatus();
                            } catch (e) {
                                showToast("Lỗi kết nối", false);
                            } finally {
                                setTimeout(() => {
                                    btn.disabled = false;
                                    btn.textContent = "Mở Đăng nhập";
                                }, 3000);
                            }
                        });
                    });

                    // Bind per-account refresh handler
                    accountsContainer.querySelectorAll(".btn-refresh-copilot-single").forEach(btn => {
                        btn.addEventListener("click", async () => {
                            const id = btn.getAttribute("data-id");
                            btn.disabled = true;
                            btn.textContent = "⏳ Đang quét...";
                            try {
                                const res = await fetch("/admin/api/copilot/refresh", {
                                    method: "POST",
                                    headers: { "Content-Type": "application/json" },
                                    body: JSON.stringify({ id })
                                });
                                const data = await res.json();
                                showToast(data.message || `Đang kích hoạt headless Chrome lấy token cho ${id}...`);
                                setTimeout(() => {
                                    fetchCopilotStatus();
                                    btn.disabled = false;
                                    btn.textContent = "⚡ Làm mới (CDP)";
                                }, 6000);
                            } catch (e) {
                                showToast("Lỗi kết nối", false);
                                btn.disabled = false;
                                btn.textContent = "⚡ Làm mới (CDP)";
                            }
                        });
                    });

                    // Bind delete handlers
                    accountsContainer.querySelectorAll(".btn-delete-copilot-acc").forEach(btn => {
                        btn.addEventListener("click", async () => {
                            const id = btn.getAttribute("data-id");
                            btn.disabled = true;
                            btn.textContent = "Đang xóa...";
                            try {
                                const delRes = await fetch("/admin/api/copilot/delete", {
                                    method: "POST",
                                    headers: { "Content-Type": "application/json" },
                                    body: JSON.stringify({ id })
                                });
                                const delData = await delRes.json();
                                if (delData.success) {
                                    showToast(`Đã xóa tài khoản ${id}!`, true);
                                    fetchCopilotStatus();
                                } else {
                                    showToast(delData.error?.message || "Lỗi khi xóa", false);
                                    btn.disabled = false;
                                    btn.textContent = "Xóa";
                                }
                            } catch (e) {
                                showToast("Lỗi kết nối", false);
                                btn.disabled = false;
                                btn.textContent = "Xóa";
                            }
                        });
                    });
                }
            }
        } catch (e) {
            console.error("fetchCopilotStatus error:", e);
        }
    }

    // Add new account slot button
    const btnCopilotAddAccount = document.getElementById("btn-copilot-add-account");
    if (btnCopilotAddAccount) {
        btnCopilotAddAccount.addEventListener("click", async () => {
            btnCopilotAddAccount.disabled = true;
            try {
                const res = await fetch("/admin/api/copilot/add", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({})
                });
                const data = await res.json();
                if (data.success) {
                    showToast(`Đã tạo slot tài khoản mới: ${data.account.id}!`, true);
                    fetchCopilotStatus();
                } else {
                    showToast(data.error?.message || "Lỗi thêm tài khoản", false);
                }
            } catch (e) {
                showToast("Lỗi kết nối", false);
            } finally {
                setTimeout(() => { btnCopilotAddAccount.disabled = false; }, 1500);
            }
        });
    }

    const btnCopilotAutoLogin = document.getElementById("btn-copilot-auto-login");
    if (btnCopilotAutoLogin) {
        btnCopilotAutoLogin.addEventListener("click", async () => {
            btnCopilotAutoLogin.disabled = true;
            btnCopilotAutoLogin.textContent = "Đang mở Chrome...";
            try {
                const res = await fetch("/admin/api/copilot/auto-login", { 
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ force: false })
                });
                const data = await res.json();
                if (data.success) {
                    showToast(data.message || "Chrome đã mở trên Ubuntu! Hãy đăng nhập tài khoản M365.", true);
                } else {
                    if (confirm(data.message + "\n\nBạn có muốn đóng Chrome cũ và mở lại không?")) {
                        const forceRes = await fetch("/admin/api/copilot/auto-login", {
                            method: "POST",
                            headers: { "Content-Type": "application/json" },
                            body: JSON.stringify({ force: true })
                        });
                        const forceData = await forceRes.json();
                        showToast(forceData.message || "Đã mở lại Chrome!", forceData.success);
                    }
                }
                fetchCopilotStatus();
            } catch (e) {
                showToast("Lỗi kết nối", false);
            } finally {
                setTimeout(() => {
                    btnCopilotAutoLogin.disabled = false;
                    btnCopilotAutoLogin.innerHTML = `<svg class="icon-small" viewBox="0 0 24 24"><path d="M19 13h-6v6h-2v-6H5v-2h6V5h2v6h6v2z"/></svg> Đăng nhập Chrome`;
                }, 3000);
            }
        });
    }

    const btnCopilotStopLogin = document.getElementById("btn-copilot-stop-login");
    if (btnCopilotStopLogin) {
        btnCopilotStopLogin.addEventListener("click", async () => {
            btnCopilotStopLogin.disabled = true;
            btnCopilotStopLogin.textContent = "Đang đóng...";
            try {
                const res = await fetch("/admin/api/copilot/auto-login/stop", { method: "POST" });
                const data = await res.json();
                showToast(data.message || "Đã đóng Chrome đăng nhập.", true);
                fetchCopilotStatus();
            } catch (e) {
                showToast("Lỗi kết nối", false);
            } finally {
                setTimeout(() => {
                    btnCopilotStopLogin.disabled = false;
                    btnCopilotStopLogin.textContent = "🛑 Dừng Chrome";
                }, 1500);
            }
        });
    }

    const btnCopilotRefresh = document.getElementById("btn-copilot-refresh");
    if (btnCopilotRefresh) {
        btnCopilotRefresh.addEventListener("click", async () => {
            btnCopilotRefresh.disabled = true;
            btnCopilotRefresh.textContent = "Đang làm mới ngầm...";
            try {
                const res = await fetch("/admin/api/copilot/refresh", { 
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({})
                });
                const data = await res.json();
                if (data.success) {
                    showToast("Đang kích hoạt headless Chrome lấy token mới cho toàn bộ pool...", true);
                    setTimeout(fetchCopilotStatus, 6000);
                } else {
                    showToast("Lỗi: " + (data.message || "Không thể làm mới"), false);
                }
            } catch (e) {
                showToast("Lỗi kết nối", false);
            } finally {
                setTimeout(() => {
                    btnCopilotRefresh.disabled = false;
                    btnCopilotRefresh.innerHTML = `<svg class="icon-small" viewBox="0 0 24 24"><path d="M12 4V1L8 5l4 4V6c3.31 0 6 2.69 6 6 0 1.01-.25 1.97-.7 2.8l1.46 1.46C19.54 15.03 20 13.57 20 12c0-4.42-3.58-8-8-8zm0 14c-3.31 0-6-2.69-6-6 0-1.01.25-1.97.7-2.8L5.24 7.74C4.46 8.97 4 10.43 4 12c0 4.42 3.58 8 8 8v3l4-4-4-4v3z"/></svg> Làm mới Tất cả`;
                }, 4000);
            }
        });
    }

    const btnSaveCopilot = document.getElementById("btn-save-copilot-token");
    if (btnSaveCopilot) {
        btnSaveCopilot.addEventListener("click", async () => {
            const input = document.getElementById("input-copilot-token");
            const val = input.value.trim();
            if (!val) {
                showToast("Vui lòng dán WebSocket URL hoặc Token", false);
                return;
            }
            const accSelect = document.getElementById("select-copilot-token-account");
            const targetId = accSelect ? accSelect.value : "";

            btnSaveCopilot.disabled = true;
            try {
                const res = await fetch("/admin/api/copilot/token", {
                    method: "POST",
                    headers: {"Content-Type": "application/json"},
                    body: JSON.stringify({id: targetId, url: val})
                });
                const data = await res.json();
                if (data.success) {
                    showToast("Lưu token Copilot thành công!", true);
                    input.value = "";
                    fetchCopilotStatus();
                } else {
                    showToast("Lỗi: " + (data.error ? data.error.message : "Token không hợp lệ"), false);
                }
            } catch (e) {
                showToast("Lỗi kết nối", false);
            } finally {
                btnSaveCopilot.disabled = false;
            }
        });
    }

    // Initialize polling
    startStatsPolling();
});
