"""Mock HTTP tool server for ReAct demo testing.

提供两个简单工具端点：
  POST /weather   — 返回"城市"的天气（mock，随机温度）
  POST /calc      — 计算 a + b

容器内通过 http://host.docker.internal:3009 访问。
"""
import json
import random
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f"[mock-tool] {self.address_string()} - {fmt % args}")

    def _read_json(self):
        length = int(self.headers.get("Content-Length", 0))
        if length == 0:
            return {}
        raw = self.rfile.read(length)
        try:
            return json.loads(raw)
        except Exception:
            return {"_raw": raw.decode("utf-8", errors="replace")}

    def _send_json(self, code, obj):
        body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        path = self.path.rstrip("/")
        if path == "/weather":
            params = self._read_json()
            city = params.get("city", "北京")
            temp = random.randint(5, 35)
            conditions = ["晴", "多云", "小雨", "大风", "阴"]
            self._send_json(200, {
                "city": city,
                "temperature": temp,
                "unit": "C",
                "conditions": random.choice(conditions),
                "humidity": random.randint(30, 90),
            })
        elif path == "/calc":
            params = self._read_json()
            try:
                a = float(params.get("a", 0))
                b = float(params.get("b", 0))
                op = params.get("op", "+")
                if op == "+":
                    r = a + b
                elif op == "-":
                    r = a - b
                elif op == "*":
                    r = a * b
                elif op == "/":
                    r = a / b if b != 0 else None
                else:
                    self._send_json(400, {"error": f"unknown op: {op}"})
                    return
                self._send_json(200, {"expression": f"{a} {op} {b}", "result": r})
            except Exception as e:
                self._send_json(400, {"error": str(e)})
        else:
            self._send_json(404, {"error": f"unknown tool: {path}"})

    def do_GET(self):
        self._send_json(200, {"status": "ok", "tools": ["/weather", "/calc"]})


if __name__ == "__main__":
    host = "0.0.0.0"
    port = 3009
    server = ThreadingHTTPServer((host, port), Handler)
    print(f"[mock-tool] listening on http://{host}:{port}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\n[mock-tool] shutting down")
        server.shutdown()
