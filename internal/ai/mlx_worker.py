# Berth's MLX worker: one local model on the Mac's GPU, with its insides
# open. berthd runs it (berth ai:mlx) and the AI gateway forwards to it, so
# apps reach it as provider "mlx" with their own Berth AI token.
#
#   POST /v1/chat/completions   OpenAI-compatible, plus "steer": pin MLP
#                               neurons to a value while the model writes
#   POST /v1/interp/contrast    which MLP neuron fires for one concept and
#                               not for related ones
#   GET  /v1/models, /healthz
#
# Neurons are the MLP's hidden units: silu(gate(x)) * up(x), the input to
# down_proj, numbered by layer and index. Pinning one to a high value at
# every position is how the model is made to "think about" what it encodes.
#
# MLX isn't meant to be driven from several threads, so one thread owns the
# model and runs jobs in turn; HTTP threads only queue them. The model is
# loaded on first use and dropped after BERTH_MLX_IDLE seconds without work.
# The worker exits when stdin closes, so it can't outlive berthd.

import gc
import json
import os
import queue
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import mlx.core as mx
import mlx.nn as nn
from mlx_lm import load, stream_generate
from mlx_lm.sample_utils import make_logits_processors, make_sampler

MODEL = os.environ.get("BERTH_MLX_MODEL", "mlx-community/Qwen3-8B-4bit")
HOST, _, PORT = os.environ.get("BERTH_MLX_ADDR", "127.0.0.1:7786").rpartition(":")
IDLE = float(os.environ.get("BERTH_MLX_IDLE", "900"))
NAME = MODEL.rsplit("/", 1)[-1]

MAX_TOKENS = 2048
MAX_CONCEPTS = 120
MAX_CONCEPT_CHARS = 200
TEMPLATE = "Let me tell you about {}"
SKIP_EDGE_LAYERS = 2  # the first and last layers are about tokens, not ideas


def log(*a):
    print("mlx:", *a, file=sys.stderr, flush=True)


# ---------------------------------------------------------------- the model


class Probe(nn.Module):
    """Wraps one layer's MLP to record or pin its neurons."""

    def __init__(self, mlp, layer):
        super().__init__()
        self.inner = mlp
        self.layer = layer

    def __call__(self, x):
        pin = hooks.pins.get(self.layer)
        if hooks.record is None and pin is None:
            return self.inner(x)
        m = self.inner
        h = nn.silu(m.gate_proj(x)) * m.up_proj(x)
        if hooks.record is not None:
            hooks.record[self.layer] = h
        if pin is not None:
            keep, value = pin
            h = h * keep.astype(h.dtype) + value.astype(h.dtype)
        return m.down_proj(h)


class Stream(nn.Module):
    """Wraps one layer to record its output, the residual stream, and add
    a direction to it."""

    def __init__(self, block, layer):
        super().__init__()
        self.inner = block
        self.layer = layer

    def __call__(self, x, *args, **kwargs):
        out = self.inner(x, *args, **kwargs)
        if hooks.streams is not None:
            hooks.streams[self.layer] = out
        push = hooks.pushes.get(self.layer)
        if push is not None:
            out = out + push.astype(out.dtype)
        return out


class Hooks:
    def __init__(self):
        self.record = None  # layer → MLP neurons in the last forward pass
        self.streams = None  # layer → residual stream in the last forward pass
        self.pins = {}  # layer → (keep mask, values) over the MLP neurons
        self.pushes = {}  # layer → vector added to the residual stream


hooks = Hooks()
model = tokenizer = None
layers = []
width = 0


def ensure_loaded():
    global model, tokenizer, layers, width
    if model is not None:
        return
    t0 = time.time()
    log("loading", MODEL)
    model, tokenizer = load(MODEL)
    layers = list(model.model.layers)
    for i, layer in enumerate(layers):
        layer.mlp = Probe(layer.mlp, i)
        model.model.layers[i] = Stream(layer, i)
    width = layers[0].mlp.inner.up_proj.weight.shape[0]  # quantized weights pack the input side, not this one
    log(f"loaded {MODEL} in {time.time() - t0:.1f}s: {len(layers)} layers × {width} neurons")


def unload():
    global model, tokenizer, layers
    model = tokenizer = None
    layers = []
    gc.collect()
    mx.clear_cache()
    log("unloaded after", int(IDLE), "idle seconds")


def set_pins(steer):
    """Turns a request's steer list into each layer's (keep, add) vectors:
    {layer, neuron, value} pins one neuron; {layer, target, others, scale}
    adds the concept's direction (its activations minus the others' mean)."""
    hooks.pins, hooks.pushes = {}, {}
    if not isinstance(steer, list) or len(steer) > 16:
        if steer:
            raise BadRequest("steer is a list of at most 16 entries")
        return
    keep, add, push = {}, {}, {}
    for s in steer:
        layer = int(s.get("layer", -1))
        if not 0 <= layer < len(layers):
            raise BadRequest(f"no layer {layer}: there are {len(layers)}")
        if "neuron" in s:
            neuron, value = int(s["neuron"]), float(s["value"])
            if not 0 <= neuron < width:
                raise BadRequest(f"no neuron {neuron}: layers have {width}")
            hot = mx.zeros((width,)).at[mx.array([neuron])].add(1.0)
            keep[layer] = keep.get(layer, mx.ones((width,))) * (1.0 - hot)
            add[layer] = add.get(layer, mx.zeros((width,))) * (1.0 - hot) + hot * value
        elif "target" in s:
            d = float(s.get("scale", 1.0)) * direction(s["target"], s.get("others"))[layer]
            push[layer] = push.get(layer, 0) + d
        else:
            raise BadRequest("each steer entry needs neuron and value, or target and others")
    hooks.pins = {layer: (keep[layer], add[layer]) for layer in keep}
    hooks.pushes = push


directions = {}  # (target, others) → per-layer direction, most recent last


def direction(target, others):
    target, others = concepts_of(target, others)
    key = (target, tuple(others))
    if key not in directions:
        remember_direction(key, mx.stack([span_activations(c)[1] for c in [target, *others]]))
    directions[key] = directions.pop(key)
    return directions[key]


def remember_direction(key, streams):
    """The concept's direction in each layer's residual stream: where it
    sits, less where its neighbours sit on average."""
    directions[key] = streams[0] - streams[1:].mean(axis=0)
    mx.eval(directions[key])
    while len(directions) > 16:
        directions.pop(next(iter(directions)))


def concepts_of(target, others):
    target = str(target or "").strip()
    others = [str(o).strip() for o in others or [] if str(o).strip()]
    others = [o for o in dict.fromkeys(others) if o.lower() != target.lower()]
    if not target:
        raise BadRequest("target is required")
    if len(others) < 3:
        raise BadRequest("others needs at least 3 related concepts to compare against")
    if len(others) > MAX_CONCEPTS or any(len(c) > MAX_CONCEPT_CHARS for c in [target, *others]):
        raise BadRequest(f"at most {MAX_CONCEPTS} others, each under {MAX_CONCEPT_CHARS} characters")
    return target, others


class BadRequest(Exception):
    pass


# ---------------------------------------------------------------- jobs


def chat(job):
    req = job.body
    messages = req.get("messages")
    if not isinstance(messages, list) or not messages:
        raise BadRequest("messages is required")
    max_tokens = min(int(req.get("max_tokens") or req.get("max_completion_tokens") or 512), MAX_TOKENS)
    temp = float(req.get("temperature", 0.7))
    top_p = float(req.get("top_p", 0.95))
    set_pins(req.get("steer"))
    try:
        prompt = tokenizer.apply_chat_template(messages, tokenize=False, add_generation_prompt=True, enable_thinking=False)
        sampler = make_sampler(temp=temp, top_p=top_p)
        # OpenAI's penalties, which keep a steered model from looping on its concept.
        processors = make_logits_processors(
            presence_penalty=float(req.get("presence_penalty") or 0) or None,
            frequency_penalty=float(req.get("frequency_penalty") or 0) or None,
            repetition_penalty=float(req.get("repetition_penalty") or 0) or None,
            frequency_context_size=64, presence_context_size=64, repetition_context_size=64,
        )
        last = None
        for r in stream_generate(model, tokenizer, prompt, max_tokens=max_tokens, sampler=sampler, logits_processors=processors):
            if job.cancelled:
                break
            last = r
            if r.text:
                job.out.put(("text", r.text))
        usage = {"prompt_tokens": last.prompt_tokens if last else 0, "completion_tokens": last.generation_tokens if last else 0}
        usage["total_tokens"] = usage["prompt_tokens"] + usage["completion_tokens"]
        job.out.put(("done", {"finish_reason": (last.finish_reason if last else None) or "stop", "usage": usage}))
    finally:
        hooks.pins, hooks.pushes = {}, {}


def span_activations(text):
    """Over the concept's tokens: each layer's MLP neurons at their
    strongest, and its residual stream on average."""
    prefix = TEMPLATE.split("{}")[0].rstrip()
    start = len(tokenizer.encode(prefix))
    ids = tokenizer.encode(TEMPLATE.format(text))
    if len(ids) <= start:
        raise BadRequest(f"nothing to read in {text!r}")
    hooks.record, hooks.streams = {}, {}
    try:
        model(mx.array(ids)[None])
        acts = mx.stack([hooks.record[i][0, start:].max(axis=0) for i in range(len(layers))]).astype(mx.float32)
        stream = mx.stack([hooks.streams[i][0, start:].mean(axis=0) for i in range(len(layers))]).astype(mx.float32)
        mx.eval(acts, stream)
        return acts, stream, len(ids)
    finally:
        hooks.record, hooks.streams = None, None


def contrast(job):
    req = job.body
    target, others = concepts_of(req.get("target"), req.get("others"))
    top = max(1, min(int(req.get("top") or 10), 50))
    t0 = time.time()
    tokens = 0
    rows, streams = [], []
    for c in [target, *others]:
        if job.cancelled:
            return
        a, st, n = span_activations(c)
        rows.append(a)
        streams.append(st)
        tokens += n
        job.out.put(("progress", len(rows)))
    acts = mx.stack(rows)  # concepts × layers × neurons
    remember_direction((target, tuple(others)), mx.stack(streams))  # so steering by it is quick
    q, o = acts[0], acts[1:]
    mean, std, most = o.mean(axis=0), o.std(axis=0), o.max(axis=0)
    # How far above the others the target is, in the others' spread. The
    # floor keeps neurons that are nearly silent for everything from winning
    # on a tiny spread.
    floor = mx.abs(acts).mean(axis=(0, 2), keepdims=True)[0] * 0.25 + 1e-3
    score = (q - mean) / (std + floor)
    score = mx.where(q > most, score, -mx.inf)  # it must beat every other concept
    n_layers = score.shape[0]
    score[:SKIP_EDGE_LAYERS] = -mx.inf
    score[n_layers - SKIP_EDGE_LAYERS:] = -mx.inf
    flat = score.reshape(-1)
    order = mx.argsort(-flat)[:top]
    mx.eval(order, flat)
    concepts = [target, *others]
    candidates = []
    for i in order.tolist():
        s = flat[i].item()
        if s == float("-inf"):
            break
        layer, neuron = divmod(i, score.shape[1])
        col = acts[:, layer, neuron].tolist()
        candidates.append({
            "layer": layer, "neuron": neuron, "score": round(s, 3),
            "target": round(col[0], 4), "others_mean": round(mean[layer, neuron].item(), 4),
            "others_max": round(most[layer, neuron].item(), 4),
            "activations": [{"concept": c, "value": round(v, 4)} for c, v in zip(concepts, col)],
        })
    job.out.put(("result", {
        "object": "interp.contrast", "model": NAME, "template": TEMPLATE,
        "layers": len(layers), "neurons_per_layer": width,
        "candidates": candidates,
        "usage": {"prompt_tokens": tokens, "completion_tokens": 0, "total_tokens": tokens},
        "seconds": round(time.time() - t0, 2),
    }))


class Job:
    def __init__(self, kind, body):
        self.kind, self.body = kind, body
        self.out = queue.Queue()
        self.cancelled = False


jobs = queue.Queue()


def model_thread():
    last = time.time()
    while True:
        try:
            job = jobs.get(timeout=30)
        except queue.Empty:
            if model is not None and time.time() - last > IDLE:
                unload()
            continue
        try:
            ensure_loaded()
            (chat if job.kind == "chat" else contrast)(job)
        except BadRequest as e:
            job.out.put(("error", (400, str(e))))
        except Exception as e:  # keep serving; the app sees why
            log("job failed:", repr(e))
            job.out.put(("error", (500, f"{type(e).__name__}: {e}")))
        last = time.time()


# ---------------------------------------------------------------- http


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def send_json(self, status, obj):
        b = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def error(self, status, msg):
        kind = "invalid_request_error" if status == 400 else "api_error"
        self.send_json(status, {"error": {"type": kind, "message": msg}})

    def do_GET(self):
        if self.path == "/healthz":
            return self.send_json(200, {"model": MODEL, "loaded": model is not None, "queued": jobs.qsize()})
        if self.path == "/v1/models":
            return self.send_json(200, {"object": "list", "data": [{"id": NAME, "object": "model", "owned_by": "mlx"}]})
        self.error(404, "not found")

    def do_POST(self):
        try:
            n = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(n) or b"{}")
            if not isinstance(body, dict):
                raise ValueError
        except ValueError:
            return self.error(400, "request body is not a JSON object")
        if self.path == "/v1/chat/completions":
            job = Job("chat", body)
        elif self.path == "/v1/interp/contrast":
            job = Job("contrast", body)
        else:
            return self.error(404, "this worker serves /v1/chat/completions and /v1/interp/contrast")
        jobs.put(job)
        try:
            if job.kind == "chat" and body.get("stream"):
                self.stream_chat(job, body)
            elif job.kind == "chat":
                self.whole_chat(job)
            else:
                self.answer(job)
        except (BrokenPipeError, ConnectionResetError):
            job.cancelled = True

    def answer(self, job):
        while True:
            kind, v = job.out.get()
            if kind == "error":
                return self.error(*v)
            if kind == "result":
                return self.send_json(200, v)

    def whole_chat(self, job):
        text = []
        while True:
            kind, v = job.out.get()
            if kind == "error":
                return self.error(*v)
            if kind == "text":
                text.append(v)
            if kind == "done":
                return self.send_json(200, {
                    "id": "chatcmpl-" + uuid.uuid4().hex, "object": "chat.completion", "created": int(time.time()), "model": NAME,
                    "choices": [{"index": 0, "message": {"role": "assistant", "content": "".join(text)}, "finish_reason": v["finish_reason"]}],
                    "usage": v["usage"],
                })

    def stream_chat(self, job, body):
        cid, created = "chatcmpl-" + uuid.uuid4().hex, int(time.time())
        started = False

        def send(obj):
            b = b"data: " + json.dumps(obj).encode() + b"\n\n"
            self.wfile.write(b"%x\r\n%s\r\n" % (len(b), b))
            self.wfile.flush()

        def chunk(delta, finish=None):
            return {"id": cid, "object": "chat.completion.chunk", "created": created, "model": NAME,
                    "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}

        while True:
            kind, v = job.out.get()
            if kind == "error" and not started:
                return self.error(*v)
            if not started:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.send_header("Transfer-Encoding", "chunked")
                self.end_headers()
                started = True
                send(chunk({"role": "assistant", "content": ""}))
            if kind == "error":
                send({"error": {"type": "api_error", "message": v[1]}})
                break
            if kind == "text":
                send(chunk({"content": v}))
            if kind == "done":
                send(chunk({}, v["finish_reason"]))
                if (body.get("stream_options") or {}).get("include_usage"):
                    send({"id": cid, "object": "chat.completion.chunk", "created": created, "model": NAME, "choices": [], "usage": v["usage"]})
                break
        b = b"data: [DONE]\n\n"
        self.wfile.write(b"%x\r\n%s\r\n0\r\n\r\n" % (len(b), b))
        self.wfile.flush()


def watch_stdin():
    sys.stdin.read()
    os._exit(0)


if __name__ == "__main__":
    threading.Thread(target=watch_stdin, daemon=True).start()
    threading.Thread(target=model_thread, daemon=True).start()
    server = ThreadingHTTPServer((HOST, int(PORT)), Handler)
    server.daemon_threads = True
    log(f"serving {MODEL} on {HOST}:{PORT}")
    server.serve_forever()
