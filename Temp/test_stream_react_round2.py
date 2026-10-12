"""同一会话第二轮：验证历史回放（tool 消息 name 从 [tool:xxx] 前缀解析）不报错。"""
import json
import urllib.request

BASE = "http://127.0.0.1:8000"

login = json.dumps({"username": "admin", "password": "Aegis@2026"}).encode("utf-8")
req = urllib.request.Request(f"{BASE}/api/v1/auth/login", data=login, headers={"Content-Type": "application/json"})
with urllib.request.urlopen(req, timeout=10) as resp:
    token = json.loads(resp.read())["data"]["access_token"]
h = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}

# 复用 session 5026（历史里有 tool 消息）
body = json.dumps({
    "session_id": 5026,
    "model": "qwen-plus",
    "content": "谢谢，那上海呢？"
}, ensure_ascii=False).encode("utf-8")
req = urllib.request.Request(f"{BASE}/api/v1/agents/1002/chat/stream", data=body, headers=h)

full_text = []
try:
    with urllib.request.urlopen(req, timeout=180) as resp:
        event = None
        data_lines = []
        for raw in resp:
            line = raw.decode("utf-8").rstrip("\n")
            if line == "":
                if event == "delta" and data_lines:
                    try:
                        d = json.loads("\n".join(data_lines))
                        full_text.append(d.get("delta", ""))
                    except Exception:
                        pass
                elif event == "error":
                    print("[ERR] SSE error:", "\n".join(data_lines))
                event = None
                data_lines = []
                continue
            if line.startswith("event:"):
                event = line[6:].strip()
            elif line.startswith("data:"):
                data_lines.append(line[5:].strip())
except urllib.error.HTTPError as e:
    print(f"[ERR] HTTP {e.code}: {e.read().decode()}")
    raise SystemExit(1)

print("[OK] 第二轮回答:", "".join(full_text))
