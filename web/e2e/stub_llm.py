# 脚本化的 OpenAI 兼容替身模型，供 web/e2e/live-run.mjs 驱动真实 Planner / Worker / SSE 链路；不是真实模型。
# Planner：第一次规划派发两项工作，之后直接结束本轮。
# Worker：每轮等待 DELAY 秒后调用 graph_overview，共 STEPS 次，再 record_fact 并以文本结束。
# 收到的引导 / 恢复说明以 record_fact「收到引导：…」回显，界面据此确认消息进入了模型会话；GET /state 查看收到的引导。
# 用法：python stub_llm.py [端口=18090] [DELAY=3] [STEPS=8]
import json, sys, threading, time, uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 18090
DELAY = float(sys.argv[2]) if len(sys.argv) > 2 else 3.0
STEPS = int(sys.argv[3]) if len(sys.argv) > 3 else 8
state = {"planner_calls": 0, "requests": 0, "guidance": []}
lock = threading.Lock()


def text_of(m):
    c = m.get("content")
    if isinstance(c, str):
        return c
    if isinstance(c, list):
        return "".join(p.get("text", "") for p in c if isinstance(p, dict))
    return ""


def call(name, args):
    return {"id": "call_" + uuid.uuid4().hex[:12], "type": "function", "function": {"name": name, "arguments": json.dumps(args, ensure_ascii=False)}}


def writer_reply(prompt):
    """章节 Writer：照着上下文写一段只复述招标要求的草稿，不声称任何企业事实，能通过写作核验。"""
    start = prompt.find("{", prompt.find("以下 JSON 为章节上下文"))
    end = prompt.rfind("\n\n只输出一个 JSON 对象")
    ctx = json.loads(prompt[start:end]) if start >= 0 and end > start else {}
    time.sleep(state.get("writer_delay", 0))
    lines = [f"## {ctx.get('title', '本章')}", "", "档案扫描按目录逐项登记，扫描后进行图像清晰度复核。数据交付提供目录索引和备份清单，按批次执行验收。"]
    for r in ctx.get("requirements") or []:
        lines.append(f"本章响应 {r.get('code')}：{r.get('title')}。")
    lines.append("（脚本替身模型输出，仅用于工程验收）")
    return json.dumps({"content_md": "\n".join(lines), "summary_note": "脚本替身模型输出", "diff_summary": "初稿", "evidence_ids": [], "admissibility_item_ids": [], "commitment_ids": [], "gaps": []}, ensure_ascii=False)


def agent_turn(tool_msgs, msgs):
    """按 /config 下发的脚本逐轮调用工具；每个工具结果都记下来，供验收断言拒绝原因。"""
    script = state["agent_script"]
    with lock:
        state["tool_results"] = [text_of(m)[:2000] for m in msgs if m.get("role") == "tool"]
    if tool_msgs < len(script):
        step = script[tool_msgs]
        return None, [call(step["name"], step["args"])]
    return "脚本替身模型：作业已提交，结果由后台事件带回。", None


def decide(body):
    tools = {t["function"]["name"] for t in body.get("tools", [])}
    msgs = body.get("messages", [])
    tool_msgs = sum(1 for m in msgs if m.get("role") == "tool")
    first_user = next((text_of(m) for m in msgs if m.get("role") == "user"), "")
    if "只输出一个 JSON 对象" in first_user and "content_md" in first_user:
        return writer_reply(first_user), None
    if "add_intent" in tools:
        if tool_msgs == 0:
            with lock:
                state["planner_calls"] += 1
                first = state["planner_calls"] <= 1
            if first:
                if state.get("agent_script"):
                    return None, [call("add_intent", {"summary": "解析招标原件并生成架构图", "rationale": "脚本替身模型的验收意图", "priority": 8})]
                return None, [
                    call("add_intent", {"summary": "核对投标人资格条款与资信证明", "rationale": "资格条款决定能否投标", "priority": 8}),
                    call("add_intent", {"summary": "核对评分表技术部分的响应要点", "rationale": "评分项影响技术标得分", "priority": 6}),
                ]
        return "规划完成：已派发资格核对与评分核对两项工作。", None
    # worker
    if state.get("agent_script"):
        return agent_turn(tool_msgs, msgs)
    last = msgs[-1] if msgs else {}
    started = any(m.get("role") == "assistant" for m in msgs)
    # 运行中的引导由 PreToolUse 拦下一次工具调用投递（工具结果以「操作员纠偏指令」开头）；
    # 暂停后「发送并恢复」的说明作为新的 user 消息到达。
    steer = text_of(last) if last.get("role") == "tool" else ""
    if "操作员纠偏指令" in steer:
        g = steer.split("：", 1)[-1].strip()
        with lock:
            state["guidance"].append(g)
        return None, [call("record_fact", {"summary": "收到引导：" + g[:60], "confidence": 0.5})]
    if started and last.get("role") == "user":
        g = text_of(last).strip()
        with lock:
            state["guidance"].append(g)
        return None, [call("record_fact", {"summary": "收到引导：" + g[:60], "confidence": 0.5})]
    if tool_msgs < STEPS:
        time.sleep(DELAY)
        return None, [call("graph_overview", {})]
    if tool_msgs == STEPS:
        return None, [call("record_fact", {"summary": "已逐条核对：资格条款均有对应资信记录（脚本模型输出，仅用于界面联调）", "confidence": 0.6})]
    return "核对完成（脚本模型输出）。", None


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_GET(self):
        if self.path == "/state":
            raw = json.dumps(state, ensure_ascii=False).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(raw))); self.end_headers(); self.wfile.write(raw)
            return
        self.send_response(404); self.end_headers()

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(n) or b"{}")
        if self.path == "/reset":
            with lock:
                state.update(planner_calls=0, requests=0, guidance=[], writer_delay=0)
            self.send_response(204); self.end_headers(); return
        if self.path == "/config":
            with lock:
                state.update(body)
            self.send_response(204); self.end_headers(); return
        if self.path.endswith("/embeddings"):
            time.sleep(state.get("embed_delay", 0))
            inputs = body.get("input") or []
            raw = json.dumps({"data": [{"index": i, "embedding": [1.0, 0.5, 0.25]} for i in range(len(inputs))], "model": body.get("model"), "usage": {"total_tokens": len(inputs)}}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(raw))); self.end_headers(); self.wfile.write(raw)
            return
        with lock:
            state["requests"] += 1
        text, calls = decide(body)
        usage = {"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}
        finish = "tool_calls" if calls else "stop"
        try:
            if body.get("stream"):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                delta = {"role": "assistant"}
                if calls:
                    delta["tool_calls"] = [dict(c, index=i) for i, c in enumerate(calls)]
                else:
                    delta["content"] = text
                for chunk in (
                    {"choices": [{"index": 0, "delta": delta, "finish_reason": None}]},
                    {"choices": [{"index": 0, "delta": {}, "finish_reason": finish}]},
                    {"choices": [], "usage": usage},
                ):
                    self.wfile.write(b"data: " + json.dumps(chunk, ensure_ascii=False).encode() + b"\n\n")
                self.wfile.write(b"data: [DONE]\n\n")
            else:
                msg = {"role": "assistant", "content": text}
                if calls:
                    msg["tool_calls"] = calls
                raw = json.dumps({"id": "stub", "object": "chat.completion", "model": body.get("model"), "choices": [{"index": 0, "message": msg, "finish_reason": finish}], "usage": usage}, ensure_ascii=False).encode()
                self.send_response(200); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(raw))); self.end_headers(); self.wfile.write(raw)
        except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError):
            pass


ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
