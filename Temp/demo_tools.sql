-- demo tools + bindings for ReAct testing

INSERT INTO tools (tool_id, name, description, type, endpoint, method, params_schema, timeout_ms, status) VALUES
  (9001, 'weather', '查询指定城市的当前天气，返回温度、天气状况、湿度等信息', 'http', 'http://host.docker.internal:3009/weather', 'POST',
   '{"type":"object","properties":{"city":{"type":"string","description":"城市名称，如 北京、上海、广州"}},"required":["city"]}'::jsonb, 5000, 1),
  (9002, 'calc', '简单计算器，支持加减乘除四则运算', 'http', 'http://host.docker.internal:3009/calc', 'POST',
   '{"type":"object","properties":{"a":{"type":"number","description":"第一个操作数"},"b":{"type":"number","description":"第二个操作数"},"op":{"type":"string","enum":["+","-","*","/"],"description":"运算符"}},"required":["a","b","op"]}'::jsonb, 3000, 1)
ON CONFLICT (name) DO UPDATE SET
  description=EXCLUDED.description,
  endpoint=EXCLUDED.endpoint,
  method=EXCLUDED.method,
  params_schema=EXCLUDED.params_schema,
  timeout_ms=EXCLUDED.timeout_ms,
  status=1;

-- 把 weather + calc 绑定到冒烟Agent（agent_id=1002）
INSERT INTO agent_tools (agent_id, tool_name, enabled) VALUES
  ('1002', 'weather', TRUE),
  ('1002', 'calc', TRUE)
ON CONFLICT (agent_id, tool_name) DO UPDATE SET enabled=TRUE;

-- 同时更新 agents 表的 tools TEXT[] 字段（PlanFrom 会读 agent.Tools）
UPDATE agents SET tools = ARRAY['weather', 'calc'] WHERE agent_id = 1002;

-- 验证
SELECT t.tool_id, t.name, t.endpoint, t.method, t.status FROM tools t ORDER BY t.tool_id;
SELECT atb.agent_id, atb.tool_name, atb.enabled FROM agent_tools atb ORDER BY atb.agent_id, atb.tool_name;
SELECT agent_id, name, tools FROM agents WHERE agent_id = 1002;
