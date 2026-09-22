# GeminiGo 🚀

**Enterprise-Grade OpenAI-Compatible AI Gateway** kết hợp hai engine mạnh mẽ: **Google Gemini Web** và **Microsoft 365 Copilot / GPT-5.6**.

Hệ thống reverse-engineer giao thức nội bộ của Gemini Web và Microsoft Copilot Substrate SignalR WebSocket — biến các tài khoản web miễn phí/doanh nghiệp thành một API server tương thích 100% chuẩn OpenAI (`/v1/chat/completions`, `/v1/images/generations`, `/v1/models`).

---

## 🌟 Tính năng nổi bật

| Tính năng | Trạng thái | Mô tả kỹ thuật |
|:---|:---:|:---|
| **Dual AI Engine** | ✅ Ổn định | Hỗ trợ song song Google Gemini 3.8 / 3.1 Pro và Microsoft Copilot / GPT-5.6 |
| **Phân bổ Pool 40/60** | ✅ Mới | 40% tài khoản cho **Session Reuse Pool** & 60% cho **Stateless Worker Pool** |
| **Mượn động (Overload Borrowing)** | ✅ Mới | Tự động mượn 1 - 2 tài khoản khi một bên pool bị quá tải hoặc dính rate-limit |
| **Nhớ ngữ cảnh qua Header** | ✅ Mới | Header `X-Session-ID` ghim tài khoản, nối tiếp hội thoại web mà không cần gửi lại lịch sử |
| **Tạo ảnh DALL-E 3 & Imagen 3** | ✅ Ổn định | Model `dall-e-3` (Copilot PNG 2048x2048) và `gemini-imagen` (Imagen 3 RPC/Playwright) |
| **Hàng đợi Request Queue 10s** | ✅ Mới | Tránh lỗi 503 "Server busy" khi tải cao, tự động xếp hàng và phục vụ ngay khi có slot |
| **Làm mới ngầm vô hình (Xvfb)** | ✅ Mới | Gia hạn token Copilot 100% ngầm trong RAM qua X Virtual Framebuffer, không bật cửa sổ desktop |
| **Multimodal Vision 16MB** | ✅ Ổn định | Hỗ trợ ảnh chụp độ nét cao qua Base64 data URI hoặc URL công khai |
| **Quản trị Web Dashboard** | ✅ Ổn định | Giám sát trạng thái pool, phân vai thủ công, theo dõi thời hạn token thời gian thực |

---

## 🤖 Danh sách Models hỗ trợ

| Model ID | Engine | Mô tả |
|---|:---:|---|
| `gemini-3.8-flash` *(Default)* | Gemini | Flagship Gemini thế hệ 3.8, tốc độ cao, đa năng (Cần tài khoản) |
| `gemini-3.8-thinking` | Gemini | Tư duy mở rộng, giải toán & phân tích logic chuyên sâu (Cần tài khoản) |
| `gemini-3.1-pro` | Gemini | Lý luận nâng cao, lập trình & suy luận phức tạp (Cần tài khoản) |
| `gemini-3.5-flash-lite` | Gemini | Flash-Lite siêu tốc, hỗ trợ cả **Guest mode (không cần đăng nhập tài khoản)** |
| `gemini-imagen` | Gemini | Tạo ảnh chất lượng cao Google Imagen 3 (Fast RPC + Playwright Fallback) |
| `gemini-veo` | Gemini | Tạo video AI chất lượng cao qua Playwright CDP |
| `gpt-5.6-think` | Copilot | OpenAI GPT-5.6 Reasoning model (Think Deeper) qua SignalR |
| `copilot-quick` / `gpt-5.6` | Copilot | Phản hồi siêu tốc qua cụm máy chủ Microsoft 365 (~4s) |
| `copilot-auto` | Copilot | Copilot tự cân bằng giữa tốc độ và chiều sâu câu trả lời |
| `dall-e-3` / `copilot-image` | Copilot | Tạo ảnh nghệ thuật DALL-E 3 gốc PNG 2048x2048 lưu tại local URL |

> 💡 **Cơ chế lọc Model tự động theo trạng thái Pool:**
> - **Khi chưa đăng nhập tài khoản Google:** Danh sách `/v1/models` tự động ẩn các model yêu cầu tài khoản (`3.8 Flash`, `Thinking`, `Pro`, `Imagen`, `Veo`), và **chỉ hiển thị duy nhất `gemini-3.5-flash-lite`** (cho phép chat ngay lập tức ở chế độ Guest).
> - **Khi đã đăng nhập tài khoản Google:** Tự động mở khóa toàn bộ danh mục model Gemini cao cấp.
> - **Tương tự với Copilot:** Các model Copilot / GPT-5.6 / DALL-E 3 chỉ hiển thị khi có ít nhất 1 tài khoản Copilot đang hoạt động.

---

## 🏗️ Kiến trúc Pool 40/60 & Tái sử dụng Phiên

```
                      [ Client Request ]
                               │
                Có Header "X-Session-ID"?
                     ├── Có ─────────────► [ Session Reuse Pool (40%) ]
                     │                      (Ghim 1 tài khoản, nối luồng web chat)
                     │
                     └── Không ──────────► [ Stateless Worker Pool (60%) ]
                                            (Xoay vòng round-robin, thông lượng cao)

       * Nếu 1 bên quá tải/cooldown ──► Tự động mượn 1 - 2 tài khoản bên kia!
```

### Nguyên tắc vận hành:
1. **Trò chuyện liên tục (Chatbot):** Truyền header `X-Session-ID: <session_id>`. Server tự ghim tài khoản và truyền metadata (`conversation_id` với Gemini, `invocationId` với Copilot). Client chỉ cần gửi đúng 1 câu hỏi mới, không cần truyền lại toàn bộ lịch sử `messages`.
2. **Batch / Automation / Tool:** Không truyền `X-Session-ID`. Server tự động xoay tua qua Worker Pool để đạt thông lượng cao nhất.
3. **Quy tắc an toàn:**
   - **Auto-Reset sau 25 Turns:** Tự động mở thread mới khi đạt 25 turns để tránh tràn context web, đồng thời vẫn giữ nguyên tài khoản đã ghim.
   - **Idle TTL 30 phút:** Tự động thu hồi phiên không hoạt động quá 30 phút để nhường tài nguyên.

---

## 🚀 Cài đặt & Vận hành

### Yêu cầu hệ thống:
- Linux (Ubuntu 22.04 / 24.04 khuyên dùng)
- Go 1.25+
- Google Chrome (`google-chrome` hoặc `chromium-browser`)
- `xvfb` (`sudo apt install xvfb -y`) để kích hoạt làm mới ngầm 100% không bật cửa sổ

### 1. Build
```bash
go build -o geminigo ./cmd/geminigo
```

### 2. Chạy dịch vụ
```bash
./geminigo --config config.json --port 8081
```
Giao diện quản trị sẽ sẵn sàng tại: `http://localhost:8081`

---

## 📡 API Reference & Ví dụ

### 1. Chat có nhớ ngữ cảnh (Multi-turn Session Reuse)

#### Turn 1:
```bash
curl -X POST http://localhost:8081/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: sess_demo_01" \
  -d '{
    "model": "gemini-3.8-flash",
    "messages": [
      {"role": "user", "content": "Thành phố tôi yêu thích là Đà Nẵng. Bạn chỉ cần trả lời: Đã nhớ."}
    ]
  }'
```

#### Turn 2 (Chỉ cần gửi câu hỏi tiếp theo):
```bash
curl -X POST http://localhost:8081/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: sess_demo_01" \
  -d '{
    "model": "gemini-3.8-flash",
    "messages": [
      {"role": "user", "content": "Thành phố tôi yêu thích là gì?"}
    ]
  }'
```
*Phản hồi:* `"Thành phố bạn yêu thích là Đà Nẵng."`

---

### 2. Tạo hình ảnh DALL-E 3 (Copilot)

#### Cách A: Endpoint tạo ảnh chuẩn (`/v1/images/generations`)
```bash
curl -X POST http://localhost:8081/v1/images/generations \
  -H "Content-Type: application/json" \
  -d '{
    "model": "dall-e-3",
    "prompt": "một chú cún corgi dễ thương ngồi trên bãi cỏ mùa thu",
    "response_format": "url"
  }'
```
*Trả về:*
```json
{
  "created": 1790046678,
  "data": [
    {
      "url": "http://localhost:8081/v1/files/img_7926301ccb34202c4c65cef7044085ef.png"
    }
  ]
}
```

#### Cách B: Gọi trong app chat (`/v1/chat/completions`)
```bash
curl -X POST http://localhost:8081/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "dall-e-3",
    "messages": [
      {"role": "user", "content": "vẽ phi thuyền vũ trụ bay qua dải ngân hà"}
    ]
  }'
```
*Trả về dạng Markdown:* `![Generated Image](http://localhost:8081/v1/files/...)` hiển thị trực tiếp trong khung chat.

---

### 3. Ví dụ Python (Thư viện `openai`)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8081/v1",
    api_key="sk-optional"
)

# 1. Trò chuyện nhớ ngữ cảnh
headers = {"X-Session-ID": "user_lan_anh"}

resp1 = client.chat.completions.create(
    model="gemini-3.8-flash",
    messages=[{"role": "user", "content": "Tôi thích ăn phở bò."}],
    extra_headers=headers
)

resp2 = client.chat.completions.create(
    model="gemini-3.8-flash",
    messages=[{"role": "user", "content": "Tôi thích ăn món gì?"}],
    extra_headers=headers
)
print("Bot trả lời:", resp2.choices[0].message.content)

# 2. Tạo ảnh DALL-E 3
img_resp = client.images.generate(
    model="dall-e-3",
    prompt="Một tách cà phê nóng phong cách tranh sơn dầu",
    n=1
)
print("Link ảnh:", img_resp.data[0].url)
```

---

## 🛠️ Cấu hình (`config.json`)

```json
{
  "port": 8081,
  "host": "0.0.0.0",
  "retry_attempts": 3,
  "retry_delay_sec": 2,
  "request_timeout_sec": 180,
  "default_model": "gemini-3.8-flash",
  "log_requests": true,
  "require_auth": false,
  "api_keys": []
}
```

---

## 📄 Bản quyền & Trách nhiệm
Mã nguồn được phân phối dưới giấy phép MIT. Dự án phục vụ mục đích nghiên cứu, học tập kỹ thuật tích hợp và tự động hóa giao thức. Tuân thủ Điều khoản sử dụng của các dịch vụ liên quan.
