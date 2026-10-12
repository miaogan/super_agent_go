#!/usr/bin/env python3
"""环境变量审计：找出「写了但读不到」的配置项。

★ 为什么需要这个工具：
   本项目 pkg/conf 的 applyEnv 采用**显式逐字段声明**
   （不用反射/通用映射）。显式的好处是行为可预测，
   代价是——只要有人写了一个「看起来对」的变量名，
   而没在applyEnv 里声明，它就会被**静默忽略**：
   不报错、不警告、不生效。

   项目里已经因此踩过两次：
     1. 部署文档整批 RAG_*/DEEPSEEK_*/EMBEDDING_* —— 代码一个都不读
     2. docker-compose 的 AEGIS_LLM__API_KEY —— LLMConf 顶层压根没这个字段

   两次都是「照抄看起来对的写法」。这个脚本把该检查变成可执行的。

两类问题必须分开报（处理方式完全不同）：
   [无字段]  变量名在配置结构体里找不到对应字段
             → 要么改对名字，要么删掉；留着纯误导
   [无映射]  字段在结构体里存在（yaml tag 能对上），
             但 applyEnv 里没有 setXxx 声明
             → 补一行 setXxx 即可支持 env 覆盖

用法：
  python3 scripts/env_audit.py            # 审计 compose / .env
  python3 scripts/env_audit.py --conf     # 只列出 conf.go 声明的变量
  python3 scripts/env_audit.py --yaml     # 审计 docs/ 里的变量名
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CONF_GO = ROOT / "pkg/conf/conf.go"

# compose 里这些是 ${VAR:-default} 插值源，不是注入容器的环境变量，
# 不该参与「是否被 conf.go 读取」的判定。
COMPOSE_INTERPOLATION = {
    "AEGIS_LLM_MODEL", "AEGIS_EMBED_MODEL", "AEGIS_JWT_SECRET",
    "AEGIS_SONIC", "AEGIS_XXX",
}

# 文档里用「AEGIS_RAG__*」这种通配写法指代整段前缀，不是具体变量名。
# 它们以「__」结尾，可据此判定为片段并放过。
PROSE_PREFIX_FRAGMENTS = {"AEGIS_RAG__", "AEGIS_LLM__", "AEGIS_AUTH__",
                          "AEGIS_AGENT__", "AEGIS_MILVUS__", "AEGIS_JSON__",
                          "AEGIS_LOG__", "AEGIS_NACOS__", "AEGIS_GATEWAY__",
                          "AEGIS_SERVER__", "AEGIS_SERVICE__", "AEGIS_POSTGRES__",
                          "AEGIS_UPSTREAM__", "AEGIS_MOCK__", "AEGIS_REDIS__",
                          "AEGIS_S3__", "AEGIS_LLM__MODELS__"}


# ---------------------------------------------------------------- 解析 conf.go

def parse_conf() -> tuple[dict[str, str], set[str]]:
    """返回 (env 变量名 → 来源, 全部 yaml 路径集合)。

    yaml 路径形如 ``rag.rerank_top_k``，用来判断「字段是否存在」。
    """
    if not CONF_GO.exists():
        sys.exit(f"找不到 {CONF_GO}")
    text = CONF_GO.read_text(encoding="utf-8")

    # ---- 1. 结构体名 → yaml 段名 ----
    # 形如：type RAGConf struct { ... }  段名取 RAGConf 去尾 Conf 后小写 → rag
    # type Config struct 是顶层，段名为空串（无前缀）。
    #
    #用 finditer + 非贪婪一次拿全部结构体，而不是为Config 写特例分支——
    # 特例分支容易在后续新增结构体时被漏掉。
    seg_of: dict[str, str] = {}
    all_structs: list[tuple[str, str]] = []
    for m in re.finditer(r"^type (\w+) struct \{(.*?)^\}", text, re.S | re.M):
        go_name, body = m.group(1), m.group(2)
        all_structs.append((go_name, body))
        seg_of[go_name] = "" if go_name == "Config" else re.sub(r"Conf$", "", go_name).lower()

    # ---- 2. 各结构体的 yaml 字段路径 ----
    yaml_paths: set[str] = set()
    for go_name, body in all_structs:
        seg = seg_of.get(go_name, go_name.lower())
        for ym in re.finditer(r'`yaml:"([^"]+)"`', body):
            field = ym.group(1).split(",")[0]
            yaml_paths.add(f"{seg}.{field}" if seg else field)

    # ---- 3. applyEnv 里声明的 env 变量名 ----
    am = re.search(r"func applyEnv\(c \*Config\) \{(.*?)\n\}\n", text, re.S)
    if not am:
        sys.exit("未能在 conf.go 中定位 applyEnv 函数体")
    env_body = am.group(1)

    declared: dict[str, str] = {}
    # ★ 必须把 setList / setDur 一起匹配：
    #   setList → 字符串切片（auth.audience、nacos.addrs、gateway.allow_origins）
    #   setDur  → time.Duration（agent.session_ttl、gateway.*_timeout）
    # 漏掉任何一个都会把「已支持 env 覆盖」的字段误报成「无映射」，
    # 而误报会让人去改本来正确的代码——比漏报更有害。
    for m in re.finditer(
            r'set(?:Str|Int|Float|Bool|Int64|List|Dur)\(\s*"([A-Z0-9_]+)"',
            env_body):
        declared[m.group(1)] = "字面量"

    # 模型索引映射：base := fmt.Sprintf("%s%d__", prefix, idx)
    #代码形如 setStr(base+"API_KEY", &m.APIKey)，展开成 <prefix>{i}__<字段>。
    model_fields = re.findall(
        r'set(?:Str|Int|Float|Bool|Int64)\(base\+"([A-Z0-9_]+)"', env_body)
    for pfx in re.findall(r'applyModelEnv\("([A-Z0-9_]+)",\s*i,', text):
        for f in model_fields:
            declared[f"{pfx}{{i}}__{f}"] = "模型索引映射"

    return declared, yaml_paths


def parse_model_count() -> int:
    """从 config.example.yaml 数出模型数量，用于展开 {i}。"""
    yml = ROOT / "configs/config.example.yaml"
    if not yml.exists():
        return 0
    txt = yml.read_text(encoding="utf-8")
    blk = re.search(r"^  models:\n((?:^    .*\n|^\n)*)", txt, re.M)
    return len(re.findall(r"^    - name:", blk.group(1), re.M)) if blk else 0


# ---------------------------------------------------------------- 采集待审计变量

def collect_env_assignments(path: Path) -> set[str]:
    """从 .env / compose 的 environment 块里取「键名」。

    只取赋值左侧的键（``KEY: value`` / ``KEY=value``），
    不取 ``${KEY:-default}`` 这类插值源——它们是 compose 自己消费的。
    """
    text = path.read_text(encoding="utf-8", errors="replace")
    keys: set[str] = set()
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        # KEY: value   （YAML 映射，compose 的 environment 块）
        m = re.match(r"^([A-Z][A-Z0-9_]*)\s*:", line)
        if m:
            keys.add(m.group(1))
            continue
        # KEY=value    （.env）
        m = re.match(r"^([A-Z][A-Z0-9_]*)=", line)
        if m:
            keys.add(m.group(1))
    return keys


def collect_prose_vars(path: Path) -> set[str]:
    """从文档散文里抓 AEGIS_* 变量名（无法区分插值，交给调用方过滤）。"""
    text = path.read_text(encoding="utf-8", errors="replace")
    found = set(re.findall(r"\bAEGIS_[A-Z0-9_]+(?:__[A-Z0-9_]+)*\b", text))
    # 滤掉通配前缀片段（如「AEGIS_RAG__* 表示这一段」）
    return {f for f in found
            if not any(f.startswith(p) and f.endswith("__") for p in PROSE_PREFIX_FRAGMENTS)}


# ---------------------------------------------------------------- 判定

def classify(var: str, declared: set[str], yaml_paths: set[str]) -> tuple[str, str]:
    """返回 (类别, 说明)。类别 ∈ {ok, no_mapping, no_field}。"""
    if var in declared:
        return "ok", ""

    # 模型索引变量：把尾部索引换回 {i} 再判
    m = re.match(r"^(AEGIS_[A-Z0-9_]+__MODELS__)\d+__(.+)$", var)
    if m and f"{m.group(1)}{{i}}__{m.group(2)}" in declared:
        return "ok", ""

    # 找它可能对应的 yaml 字段：变量名 → 段前缀 → 字段名
    # AEGIS_RAG__RERANK_TOP_K → rag.rerank_top_k（AEGIS_ 去掉、__ 换 .、小写）
    body = var[len("AEGIS_"):] if var.startswith("AEGIS_") else var
    seg, _, field = body.partition("__")
    seg = seg.lower()
    # 段名可能带尾Conf 之类的后缀差异，做宽松匹配
    cand = {f"{seg}.{field.lower()}"}
    if seg.lower() == "llm" and field.lower() == "api_key":
        cand |= {f"llm.models.{i}.api_key" for i in range(8)}
    if seg.lower() == "llm" and field.lower() == "base_url":
        cand |= {f"llm.models.{i}.base_url" for i in range(8)}

    for c in cand:
        if c in yaml_paths:
            return "no_mapping", f"对应配置项 {c} 存在，但 applyEnv 未声明 env 映射"
    return "no_field", "配置结构体里没有对应字段，改名或删除"


# ---------------------------------------------------------------- 主流程

def audit(files: list[Path], declared: dict[str, str], yaml_paths: set[str],
          prose: bool) -> int:
    expanded = set(declared)
    n = parse_model_count() or 8
    for k in declared:
        if "{i}" in k:
            for i in range(n):
                expanded.add(k.replace("{i}", str(i)))

    problems = 0
    for path in files:
        if not path.exists():
            print(f"[跳过    ] {path.relative_to(ROOT)} 不存在")
            continue
        keys = collect_prose_vars(path) if prose else collect_env_assignments(path)
        if not prose:
            keys = {k for k in keys if k.startswith("AEGIS_")}
            keys -= COMPOSE_INTERPOLATION

        buckets: dict[str, list[tuple[str, str]]] = {"no_field": [], "no_mapping": []}
        for v in sorted(keys):
            kind, why = classify(v, expanded, yaml_paths)
            if kind != "ok":
                buckets[kind].append((v, why))

        n_bad = len(buckets["no_field"]) + len(buckets["no_mapping"])
        tag = "OK" if n_bad == 0 else f"{n_bad} 处问题"
        print(f"[{tag:>10}] {path.relative_to(ROOT)}  （{len(keys)} 个 AEGIS_* 变量）")
        for kind, label, fix in (
            ("no_field", "无对应字段", "改名或从文件里删掉——留着只会误导"),
            ("no_mapping", "字段存在但无 env 映射", "在 conf.go applyEnv 里补一行 setXxx"),
        ):
            for v, why in buckets[kind]:
                print(f"    ✗ [{label}] {v}")
                print(f"        {why}")
                if fix:
                    print(f"        → {fix}")
        print()
        problems += n_bad
    return problems


def main() -> None:
    declared, yaml_paths = parse_conf()
    args = sys.argv[1:]

    if "--conf" in args:
        for k in sorted(declared):
            print(f"{k:48s} [{declared[k]}]")
        print(f"\n共 {len(declared)} 个（模型索引未展开）")
        return 0

    targets = [ROOT / "docker-compose.yml", ROOT / ".env", ROOT / ".env.example"]
    prose = "--yaml" in args
    if "--yaml" in args:
        targets = sorted((ROOT / "docs").glob("*.md"))
        prose = True

    print(f"conf.go 声明 {len(declared)} 个 env 变量，"
          f"配置结构体共 {len(yaml_paths)} 个 yaml 字段\n")
    total = audit(targets, declared, yaml_paths, prose)
    if total:
        print(f"合计 {total} 处问题。")
    else:
        print("未发现问题。")
    # ★ 必须返回非零退出码，否则 make / CI 不会失败，
    #   这个目标就只是个「打印给人看」的摆设。
    return 1 if total else 0


if __name__ == "__main__":
    sys.exit(main() or 0)