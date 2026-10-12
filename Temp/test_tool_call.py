import json, urllib.request, ssl

url = "https://llm-yghp3j032j41n5kw.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/chat/completions"
api_key = "sk-0f617146ebbb436ca1aa39d7ae00aef2"

payload = {
    "model": "qwen-plus",
    "messages": [
        {"role": "user", "content": "北京今天天气怎么样？"}
    ],
    "tools": [
        {
            "type": "function",
            "function": {
                "name": "weather",
                "description": "查询指定城市的当前天气",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "city": {"type": "string", "description": "城市名称"}
                    },
                    "required": ["city"]
                }
            }
        }
    ]
}

req = urllib.request.Request(
    url,
    data=json.dumps(payload).encode(),
    headers={
        "Content-Type": "application/json",
        "Authorization": f"Bearer {api_key}"
    }
)

ctx = ssl.create_default_context()
with urllib.request.urlopen(req, context=ctx, timeout=30) as resp:
    data = json.loads(resp.read())
    print("=== FULL RESPONSE ===")
    print(json.dumps(data, indent=2, ensure_ascii=False))
    msg = data.get("choices", [{}])[0].get("message", {})
    print("\n=== MESSAGE ===")
    print(f"content: {msg.get('content')}")
    print(f"tool_calls: {msg.get('tool_calls')}")
    print(f"finish_reason: {data.get('choices', [{}])[0].get('finish_reason')}")
