"""Clean ReAct test via Aegis gateway with proper UTF-8 encoding."""
import json, urllib.request, sys

BASE = "http://127.0.0.1:8000"

# 1) Login
login = json.dumps({"username": "admin", "password": "Aegis@2026"}).encode("utf-8")
req = urllib.request.Request(f"{BASE}/api/v1/auth/login", data=login, headers={"Content-Type": "application/json"})
with urllib.request.urlopen(req, timeout=10) as resp:
    token = json.loads(resp.read())["data"]["access_token"]
print(f"[1] logged in, token ok")

h = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}

# 2) Create fresh session
body = json.dumps({"agent_id": 1002, "title": "py-clean-test"}).encode("utf-8")
req = urllib.request.Request(f"{BASE}/api/v1/sessions", data=body, headers=h)
with urllib.request.urlopen(req, timeout=10) as resp:
    sid = json.loads(resp.read())["data"]["session_id"]
print(f"[2] created session_id={sid}")

# 3) Chat
question = "北京今天天气怎么样？请查询 weather 工具告诉我答案。"
body = json.dumps({
    "session_id": sid,
    "model": "qwen-plus",
    "content": question
}, ensure_ascii=False).encode("utf-8")
print(f"[3] sending: {question}")
req = urllib.request.Request(f"{BASE}/api/v1/agents/1002/chat", data=body, headers=h)
try:
    with urllib.request.urlopen(req, timeout=120) as resp:
        data = json.loads(resp.read())
    print(f"[4] response content: {data['data']['message']['content']}")
    print(f"[4] usage: {data['data']['message']['usage']}")
except urllib.error.HTTPError as e:
    print(f"[4] HTTP error {e.code}: {e.read().decode()}")
except Exception as e:
    print(f"[4] error: {e}")
