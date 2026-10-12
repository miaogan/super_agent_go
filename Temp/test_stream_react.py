"""流式 ChatStream ReAct 端到端测试：验证 SSE 路径工具调用循环。"""
import json
import urllib.request

BASE = "http://127.0.0.1:8000"

# 1) Login
login = json.dumps({"username": "admin", "password": "Aegis@2026"}).encode("utf-8")
req = urllib.request.Request(f"{BASE}/api/v1/auth/login", data=login, headers={"Content-Type": "application/json"})
with urllib.request.urlopen(req, timeout=10) as resp:
    token = json.loads(resp.read())["data"]["access_token"]
print("[1] logged in")

h = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}

# 2) Create fresh session
body = json.dumps({"agent_id": 1002, "title": "stream-react-test"}).encode("utf-8")
req = urllib.request.Request(f"{BASE}/api/v1/sessions", data=body, headers=h)
with urllib.request.urlopen(req, timeout=10) as resp:
    sid = json.loads(resp.read())["data"]["session_id"]
print(f"[2] session_id={sid}")

# 3) Stream chat
question = "北京今天天气怎么样？请查询 weather 工具告诉我答案。"
body = json.dumps({
    "session_id": sid,
    "model": "qwen-plus",
    "content": question
}, ensure_ascii=False).encode("utf-8")
print(f"[3] streaming: {question}")

req = urllib.request.Request(f"{BASE}/api/v1/agents/1002/chat/stream", data=body, headers=h)
evt_count = {}
full_text = []
try:
    with urllib.request.urlopen(req, timeout=180) as resp:
        event = None
        data_lines = []
        for raw in resp:
            line = raw.decode("utf-8").rstrip("\n")
            if line == "":
                if event and data_lines:
                    payload = "\n".join(data_lines)
                    evt_count[event] = evt_count.get(event, 0) + 1
                    try:
                        d = json.loads(payload)
                    except Exception:
                        d = {}
                    if event == "delta":
                        full_text.append(d.get("delta", ""))
                    elif event == "done":
                        print(f"[4] done event, msg_id={d.get('msg_id')}, content={d.get('content', '')}")
                event = None
                data_lines = []
                continue
            if line.startswith("event:"):
                event = line[6:].strip()
            elif line.startswith("data:"):
                data_lines.append(line[5:].strip())
except urllib.error.HTTPError as e:
    print(f"[4] HTTP error {e.code}: {e.read().decode()}")
    raise SystemExit(1)
except Exception as e:
    print(f"[4] error: {e}")
    raise SystemExit(1)

print(f"[5] events: {evt_count}")
print(f"[6] final text: {''.join(full_text)}")
