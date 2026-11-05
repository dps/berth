# Berth's MLX worker: one local model on the Mac's GPU, with its insides
# open. berthd runs it (berth ai:mlx) and the AI gateway forwards to it, so
# apps reach it as provider "mlx" with their own Berth AI token.
#
#   POST /v1/chat/completions   OpenAI-compatible, plus "steer": pin MLP
#                               neurons or clamp features to a value while
#                               the model writes
#   POST /v1/interp/contrast    which MLP neuron (or, with "features", which
#                               sparse-autoencoder feature) fires for one
#                               concept and not for related ones, at a chosen
#                               depth, and what each one writes to the next
#                               token
#   POST /v1/interp/trace       token by token: chosen neurons and features,
#                               each token's strongest features, how much of a
#                               concept's direction each token carries, each
#                               layer's strongest neurons; with "map", every
#                               neuron in the model as one byte; with "lens",
#                               what each layer would predict; with
#                               "attention", where one layer's heads looked
#   POST /v1/interp/scan        many texts at once: where chosen neurons (or
#                               features) fire hardest, for a page of
#                               examples per neuron
#   POST /v1/interp/patch       causal tracing: copy one layer's activations at
#                               one token from a clean prompt into a corrupted
#                               one, for every layer and token, and see how
#                               much of the clean answer comes back
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

import base64
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
import numpy as np
from mlx_lm import load, stream_generate
from mlx_lm.sample_utils import make_logits_processors, make_sampler

MODEL = os.environ.get("BERTH_MLX_MODEL", "mlx-community/Qwen3-8B-4bit")
HOST, _, PORT = os.environ.get("BERTH_MLX_ADDR", "127.0.0.1:7786").rpartition(":")
IDLE = float(os.environ.get("BERTH_MLX_IDLE", "900"))
NAME = MODEL.rsplit("/", 1)[-1]
SAE = os.environ.get("BERTH_MLX_SAE", "")  # a Hugging Face repo of the model's sparse autoencoders
SAE_DIR = os.environ.get("BERTH_MLX_SAE_DIR", "")  # their layers, converted (--get-sae)

MAX_TOKENS = 2048
MAX_CONCEPTS = 120
MAX_CONCEPT_CHARS = 200
MAX_TRACE_TOKENS = 512
MAX_SCAN_TEXTS = 512
MAX_SCAN_TOKENS = 64
SCAN_BATCH = 16
MAX_PATCH_TOKENS = 64
MAP_BYTES = 2_000_000  # the whole-model map for every token is pooled to fit
TEMPLATE = "Let me tell you about {}"
SKIP_EDGE_LAYERS = 2  # the first and last layers are about tokens, not ideas
WRITER_Z = 8  # a neuron whose top word stands this far out writes it
SAE_KEPT = 2  # layers of features in memory at once, about 1 GB each


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
        record = hooks.record is not None and (hooks.record_only is None or self.layer in hooks.record_only)
        if not record and pin is None:
            return patched(self.inner(x), "mlp", self.layer)
        m = self.inner
        h = nn.silu(m.gate_proj(x)) * m.up_proj(x)
        if record:
            hooks.record[self.layer] = h
        if pin is not None:
            keep, value = pin
            h = h * keep.astype(h.dtype) + value.astype(h.dtype)
        return patched(m.down_proj(h), "mlp", self.layer)


class Stream(nn.Module):
    """Wraps one layer to record its output, the residual stream, add a
    direction to it, and clamp features in it."""

    def __init__(self, block, layer):
        super().__init__()
        self.inner = block
        self.layer = layer

    def __call__(self, x, *args, **kwargs):
        out = self.inner(x, *args, **kwargs)
        push = hooks.pushes.get(self.layer)
        if push is not None:
            out = out + push.astype(out.dtype)
        clamp = hooks.clamps.get(self.layer)
        if clamp is not None:
            out = clamp(out)
        out = patched(out, "stream", self.layer)
        if hooks.streams is not None:
            hooks.streams[self.layer] = out
        return out


class Eyes(nn.Module):
    """Wraps one layer's attention to record where each token looked, and
    to patch its output."""

    def __init__(self, attn, layer):
        super().__init__()
        self.inner = attn
        self.layer = layer

    def __call__(self, x, mask=None, cache=None):
        if hooks.look == self.layer:
            hooks.looked = attention_pattern(self.inner, x)
        return patched(self.inner(x, mask, cache), "attn", self.layer)


def attention_pattern(a, x):
    """Recomputes the softmax the fused attention kernel keeps to itself:
    batch × heads × query × key."""
    if not all(hasattr(a, n) for n in ("q_proj", "k_proj", "rope", "n_heads", "n_kv_heads", "scale")):
        raise BadRequest(f"{NAME}'s attention isn't laid out the way this worker reads it")
    B, T, _ = x.shape
    q = a.q_proj(x).reshape(B, T, a.n_heads, -1)
    k = a.k_proj(x).reshape(B, T, a.n_kv_heads, -1)
    if hasattr(a, "q_norm"):
        q, k = a.q_norm(q), a.k_norm(k)
    q, k = a.rope(q.transpose(0, 2, 1, 3)), a.rope(k.transpose(0, 2, 1, 3))
    k = mx.repeat(k, a.n_heads // a.n_kv_heads, axis=1)
    s = ((q * a.scale) @ k.transpose(0, 1, 3, 2)).astype(mx.float32)
    future = mx.triu(mx.ones((T, T), dtype=mx.bool_), k=1)
    return mx.softmax(mx.where(future, -mx.inf, s), axis=-1)


def patched(out, site, layer):
    """Records a site's output for patching, or swaps in recorded values
    where the patch's mask says."""
    if hooks.capture is None and not hooks.patch:
        return out
    if hooks.capture is not None:
        hooks.capture[(site, layer)] = out
    p = hooks.patch.get((site, layer))
    if p is not None:
        mask, values = p
        out = mx.where(mask, values.astype(out.dtype), out)
    return out


class Hooks:
    def __init__(self):
        self.record = None  # layer → MLP neurons in the last forward pass
        self.record_only = None  # the layers to record, if not all
        self.streams = None  # layer → residual stream in the last forward pass
        self.pins = {}  # layer → (keep mask, values) over the MLP neurons
        self.pushes = {}  # layer → vector added to the residual stream
        self.clamps = {}  # layer → function setting features in the residual stream
        self.look = None  # the layer whose attention pattern to record
        self.looked = None  # and the pattern
        self.capture = None  # (site, layer) → output, for patching
        self.patch = {}  # (site, layer) → (mask, values) swapped into that output


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
        layer.self_attn = Eyes(layer.self_attn, i)
        model.model.layers[i] = Stream(layer, i)
    width = layers[0].mlp.inner.up_proj.weight.shape[0]  # quantized weights pack the input side, not this one
    log(f"loaded {MODEL} in {time.time() - t0:.1f}s: {len(layers)} layers × {width} neurons")


def unload():
    global model, tokenizer, layers
    model = tokenizer = None
    token_text.clear()
    directions.clear()
    saes.clear()
    layers = []
    gc.collect()
    mx.clear_cache()
    log("unloaded after", int(IDLE), "idle seconds")


def head(h):
    """Logits from the final norm's output."""
    lm = getattr(model, "lm_head", None)
    return lm(h) if lm is not None else model.model.embed_tokens.as_linear(h)


def writes(pairs, k=6):
    """What each neuron says straight to the next token when it fires: its
    down_proj column through the final norm and the head. Gives the words
    it raises most, and how far the top one stands out from the rest of
    the vocabulary, in standard deviations. Most neurons' top word stands
    about 5 out, which is noise; a writer's stands well past WRITER_Z, and
    pinned high, the model just says that word over and over."""
    out = []
    for b in range(0, len(pairs), 64):
        cols = []
        for layer, neuron in pairs[b:b + 64]:
            down = layers[layer].mlp.inner.down_proj
            one = mx.zeros((1, width), dtype=getattr(down, "scales", down.weight).dtype)
            cols.append(down(one.at[0, neuron].add(1)))
        out += says_of(mx.concatenate(cols), k)
    return out


def says_of(vectors, k=6):
    """The words each residual-stream vector raises most when added to the
    stream, and how far the top one stands out (see writes)."""
    logits = head(model.model.norm(vectors.astype(mx.bfloat16))).astype(mx.float32)
    top = mx.argsort(-logits, axis=1)[:, :k]
    z = (logits.max(axis=1) - logits.mean(axis=1)) / (logits.std(axis=1) + 1e-6)
    mx.eval(top, z)
    return [{"says": [decoded(t) for t in row], "says_z": round(z[i].item(), 1)} for i, row in enumerate(top.tolist())]


# ---------------------------------------------------------------- features
#
# A sparse autoencoder rewrites one layer's residual stream as a few of
# many features (Qwen-Scope's for Qwen3 8B: 50 of 65,536 at each token),
# each a direction the model uses for one thing. A neuron mixes many
# things; a feature mostly doesn't. Clamped alone, one still goes from no
# change to gibberish with little between, but a concept's few strongest
# clamped together make the model talk about it. Features are trained
# for a model, not part of it: berth ai:mlx --sae fetches a
# repo of them, one layer at a time, and --get-sae turns each layer into
# MLX's format here. Units are the feature's activation; most fire at a
# few to a few dozen.


class Features:
    """One layer's sparse autoencoder: a top-k encoder, and each feature's
    direction in the stream (W_dec's rows)."""

    def __init__(self, path, k):
        w = mx.load(path)
        self.W_enc, self.b_enc, self.W_dec = w["W_enc"], w["b_enc"], w["W_dec"]
        self.k = k
        self.n = self.W_enc.shape[0]

    def pre(self, x):
        return (x.astype(mx.bfloat16) @ self.W_enc.T).astype(mx.float32) + self.b_enc

    def encode(self, x):
        """Every feature's activation, zero for all but each token's k
        strongest: what the autoencoder was trained to read."""
        pre = self.pre(x)
        kth = mx.partition(pre, kth=self.n - self.k, axis=-1)[..., self.n - self.k:self.n - self.k + 1]
        return mx.where((pre >= kth) & (pre > 0), pre, 0)

    def clamp(self, idx, values):
        """Sets features to values at every token, as Scaling
        Monosemanticity steers: the stream moves along each feature's
        direction by what it takes, and what the autoencoder can't
        reconstruct stays as it was."""
        idx, values = mx.array(idx), mx.array(values, dtype=mx.float32)
        dirs = self.W_dec[idx].astype(mx.float32)  # m × d

        def f(out):
            now = self.encode(out)[..., idx]
            return out + ((values - now) @ dirs).astype(out.dtype)
        return f


saes = {}  # layer → Features, most recently used last
sae_config = None


def sae_layers(d=SAE_DIR):
    """The layers whose features are ready here."""
    if not d or not os.path.isdir(d):
        return []
    return sorted(int(f[5:-12]) for f in os.listdir(d) if f.startswith("layer") and f.endswith(".safetensors"))


def features_at(layer):
    global sae_config
    if layer in saes:
        saes[layer] = saes.pop(layer)
        return saes[layer]
    ready = sae_layers()
    if not SAE:
        raise BadRequest("this worker has no features: berth ai:mlx --sae <hugging-face-repo> adds a sparse autoencoder's")
    if layer not in ready:
        raise BadRequest(f"features are ready at layers {', '.join(map(str, ready)) or 'none'}; berth ai:mlx --sae-layers adds more")
    if sae_config is None:
        with open(os.path.join(SAE_DIR, "config.json")) as f:
            sae_config = json.load(f)
    while len(saes) >= SAE_KEPT:
        saes.pop(next(iter(saes)))
        gc.collect()
    saes[layer] = Features(os.path.join(SAE_DIR, f"layer{layer}.safetensors"), int(sae_config["k"]))
    if saes[layer].W_enc.shape[1] != model.model.norm.weight.shape[0]:
        saes.pop(layer)
        raise BadRequest(f"{SAE}'s features are for a model {sae_config.get('d_model')} wide; {NAME} isn't")
    return saes[layer]


def default_feature_layer():
    """The ready layer nearest the middle of the model, where features are
    most about ideas."""
    ready = sae_layers()
    if not ready:
        features_at(-1)  # says why
    return min(ready, key=lambda l: abs(l - len(layers) // 2))


def similar(layer, feature, k=5):
    """The features whose directions are nearest this one's: often the
    same idea split finer, or its neighbours."""
    d = features_at(layer).W_dec
    v = d[feature]
    cos = (d @ v).astype(mx.float32) / (mx.linalg.norm(d.astype(mx.float32), axis=-1) * mx.linalg.norm(v.astype(mx.float32)) + 1e-6)
    near = mx.argsort(-cos)[:k + 1].tolist()
    return [{"feature": f, "cos": round(cos[f].item(), 3)} for f in near if f != feature][:k]


def features_of(v, most=16):
    """{layer, feature} entries, checked."""
    out = []
    for n in v:
        layer, feature = int(n.get("layer", -1)), int(n.get("feature", -1))
        if not 0 <= feature < features_at(layer).n:
            raise BadRequest(f"no feature {feature} at layer {layer}")
        out.append((layer, feature))
    return out


def get_sae(repo, dest, want):
    """--get-sae: downloads a repo's layers and converts each to MLX bf16
    safetensors, deleting the download. Qwen-Scope's are PyTorch pickles,
    which can run code when loaded the usual way; this reads them with an
    unpickler that only knows how a tensor is saved."""
    from huggingface_hub import hf_hub_download
    os.makedirs(dest, exist_ok=True)
    cfg = json.load(open(hf_hub_download(repo, "config.json", local_dir=dest)))
    if cfg.get("model_type") != "topk_sae":
        sys.exit(f"{repo} isn't a top-k sparse autoencoder this worker reads (model_type {cfg.get('model_type')!r})")
    want = want or [cfg["num_layers"] // 2]
    for layer in want:
        out = os.path.join(dest, f"layer{layer}.safetensors")
        if os.path.exists(out):
            continue
        print(f"-----> Getting layer {layer}'s features (about 2 GB, once)", flush=True)
        pt = hf_hub_download(repo, f"layer{layer}.sae.pt", local_dir=dest)
        try:
            t = pt_tensors(pt, ("W_enc", "b_enc", "W_dec"))
            w = {"W_enc": mx.array(t["W_enc"]).astype(mx.bfloat16), "b_enc": mx.array(t["b_enc"]),
                 "W_dec": mx.array(np.ascontiguousarray(t["W_dec"].T)).astype(mx.bfloat16)}
            del t
            part = os.path.join(dest, f".layer{layer}.partial.safetensors")
            mx.save_safetensors(part, w)
            os.replace(part, out)
        finally:
            os.remove(pt)
    print("-----> Features ready at layers", ", ".join(map(str, sae_layers(dest))), flush=True)


def pt_tensors(path, names):
    """Float tensors from a PyTorch zip of a dict of tensors, without torch
    and without running anything from the pickle."""
    import pickle
    import zipfile

    class Rebuilt(tuple):
        pass

    def rebuild(storage, offset, size, stride, *rest):
        return Rebuilt((storage, offset, tuple(size), tuple(stride)))

    class Safe(pickle.Unpickler):
        def find_class(self, module, name):
            if (module, name) == ("torch._utils", "_rebuild_tensor_v2"):
                return rebuild
            if (module, name) == ("torch", "FloatStorage"):
                return name
            if (module, name) == ("collections", "OrderedDict"):
                return dict
            raise pickle.UnpicklingError(f"won't load {module}.{name}")

        def persistent_load(self, pid):
            if pid[0] != "storage" or pid[1] != "FloatStorage":
                raise pickle.UnpicklingError(f"won't load storage {pid[:2]}")
            return pid[2]

    z = zipfile.ZipFile(path)
    pkl = next(n for n in z.namelist() if n.endswith("/data.pkl"))
    d = Safe(z.open(pkl)).load()
    out = {}
    for name in names:
        key, offset, shape, stride = d[name]
        member = pkl[:-len("data.pkl")] + "data/" + key
        if z.getinfo(member).compress_type != zipfile.ZIP_STORED:
            raise ValueError(f"{name} is compressed")
        with z.open(member) as f:
            a = np.frombuffer(f.read(), dtype=np.float32)
        out[name] = np.lib.stride_tricks.as_strided(a[offset:], shape=shape, strides=[s * 4 for s in stride])
    return out


def last_logits(ids):
    """The next-token logits at the last position of each row: the model
    without its head on every other position."""
    return head(model.model(ids)[:, -1]).astype(mx.float32)


token_text = {}


def decoded(i):
    if i not in token_text:
        token_text[i] = tokenizer.decode([i])
    return token_text[i]


def unpin():
    hooks.pins, hooks.pushes, hooks.clamps = {}, {}, {}


def set_pins(steer):
    """Turns a request's steer list into each layer's (keep, add) vectors:
    {layer, neuron, value} pins one neuron; {layer, target, others, scale}
    adds the concept's direction (its activations minus the others' mean);
    {layer, feature, value} clamps a feature."""
    unpin()
    if not isinstance(steer, list) or len(steer) > 16:
        if steer:
            raise BadRequest("steer is a list of at most 16 entries")
        return
    keep, add, push, clamp = {}, {}, {}, {}
    for s in steer:
        layer = int(s.get("layer", -1))
        if not 0 <= layer < len(layers):
            raise BadRequest(f"no layer {layer}: there are {len(layers)}")
        if "feature" in s:
            (_, feature), = features_of([s])
            clamp.setdefault(layer, {})[feature] = float(s["value"])
        elif "neuron" in s:
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
            raise BadRequest("each steer entry needs neuron and value, feature and value, or target and others")
    hooks.pins = {layer: (keep[layer], add[layer]) for layer in keep}
    hooks.pushes = push
    hooks.clamps = {layer: features_at(layer).clamp(list(c), list(c.values())) for layer, c in clamp.items()}


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


def depth_of(v, n_layers):
    """The layers to search, from a [from, to] fraction of the model's depth;
    by default all but the edge layers."""
    if v is None:
        return SKIP_EDGE_LAYERS, n_layers - SKIP_EDGE_LAYERS
    if not (isinstance(v, list) and len(v) == 2 and 0 <= float(v[0]) < float(v[1]) <= 1):
        raise BadRequest("depth is [from, to], fractions of the model's depth with 0 ≤ from < to ≤ 1")
    return int(float(v[0]) * n_layers), max(int(float(v[0]) * n_layers) + 1, round(float(v[1]) * n_layers))


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
        unpin()


def span_activations(text, feat=None):
    """Over the concept's tokens: each layer's MLP neurons at their
    strongest, and its residual stream on average; with feat, a (layer,
    Features), that layer's features at their strongest too."""
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
        peaks = feat[1].encode(hooks.streams[feat[0]][0, start:]).max(axis=0) if feat else None
        mx.eval(acts, stream, peaks)
        return acts, stream, len(ids), peaks
    finally:
        hooks.record, hooks.streams = None, None


def contrast(job):
    req = job.body
    target, others = concepts_of(req.get("target"), req.get("others"))
    top = max(1, min(int(req.get("top") or 10), 50))
    feat = None
    if req.get("features"):
        layer = default_feature_layer() if req.get("layer") is None else int(req["layer"])
        feat = (layer, features_at(layer))
    t0 = time.time()
    tokens = 0
    rows, streams, peaks = [], [], []
    for c in [target, *others]:
        if job.cancelled:
            return
        a, st, n, p = span_activations(c, feat)
        rows.append(a)
        streams.append(st)
        peaks.append(p)
        tokens += n
        job.out.put(("progress", len(rows)))
    remember_direction((target, tuple(others)), mx.stack(streams))  # so steering by it is quick
    usage = {"prompt_tokens": tokens, "completion_tokens": 0, "total_tokens": tokens}
    if feat:
        return job.out.put(("result", feature_contrast(feat, [target, *others], mx.stack(peaks), top, usage, t0)))
    acts = mx.stack(rows)  # concepts × layers × neurons
    q, o = acts[0], acts[1:]
    mean, std, most = o.mean(axis=0), o.std(axis=0), o.max(axis=0)
    # How far above the others the target is, in the others' spread. The
    # floor keeps neurons that are nearly silent for everything from winning
    # on a tiny spread.
    floor = mx.abs(acts).mean(axis=(0, 2), keepdims=True)[0] * 0.25 + 1e-3
    score = (q - mean) / (std + floor)
    score = mx.where(q > most, score, -mx.inf)  # it must beat every other concept
    n_layers = score.shape[0]
    lo, hi = depth_of(req.get("depth"), n_layers)
    score[:lo] = -mx.inf
    score[hi:] = -mx.inf
    # Writers say the concept's word rather than think about it, so they're
    # left out if asked. Leaving some out needs spares to fill their places.
    keep_writers = req.get("writers", True) is not False
    flat = score.reshape(-1)
    order = mx.argsort(-flat)[:top if keep_writers else top * 8]
    mx.eval(order, flat)
    pairs = [divmod(i, score.shape[1]) for i in order.tolist() if flat[i].item() != float("-inf")]
    said = writes(pairs)
    concepts = [target, *others]
    candidates = []
    for (layer, neuron), w in zip(pairs, said):
        if not keep_writers and w["says_z"] >= WRITER_Z:
            continue
        col = acts[:, layer, neuron].tolist()
        candidates.append({
            "layer": layer, "neuron": neuron, "score": round(flat[layer * score.shape[1] + neuron].item(), 3),
            "target": round(col[0], 4), "others_mean": round(mean[layer, neuron].item(), 4),
            "others_max": round(most[layer, neuron].item(), 4),
            **w,
            "activations": [{"concept": c, "value": round(v, 4)} for c, v in zip(concepts, col)],
        })
        if len(candidates) == top:
            break
    job.out.put(("result", {
        "object": "interp.contrast", "model": NAME, "template": TEMPLATE,
        "layers": len(layers), "neurons_per_layer": width,
        "candidates": candidates,
        "usage": usage,
        "seconds": round(time.time() - t0, 2),
    }))


def feature_contrast(feat, concepts, acts, top, usage, t0):
    """contrast over one layer's features rather than every neuron. Most
    features are silent for most concepts, so the floor is a share of a
    typical firing rather than of the average, which is nearly zero."""
    layer, sae = feat
    q, o = acts[0], acts[1:]  # acts: concepts × features
    mean, std, most = o.mean(axis=0), o.std(axis=0), o.max(axis=0)
    fired = acts > 0
    floor = 0.25 * (acts.sum() / mx.maximum(fired.sum(), 1)) + 1e-3
    score = mx.where(q > most, (q - mean) / (std + floor), -mx.inf)
    order = mx.argsort(-score)[:top]
    mx.eval(order, score)
    ids = [i for i in order.tolist() if score[i].item() != float("-inf")]
    said = says_of(sae.W_dec[mx.array(ids)]) if ids else []
    candidates = []
    for f, w in zip(ids, said):
        col = acts[:, f].tolist()
        candidates.append({
            "layer": layer, "feature": f, "score": round(score[f].item(), 3),
            "target": round(col[0], 4), "others_mean": round(mean[f].item(), 4), "others_max": round(most[f].item(), 4),
            **w,
            "activations": [{"concept": c, "value": round(v, 4)} for c, v in zip(concepts, col)],
        })
    return {"object": "interp.contrast", "model": NAME, "template": TEMPLATE, "layers": len(layers),
            "features": {"repo": SAE, "layer": layer, "per_layer": sae.n, "k": sae.k, "ready": sae_layers()},
            "candidates": candidates, "usage": usage, "seconds": round(time.time() - t0, 2)}


def trace(job):
    req = job.body
    if req.get("messages"):
        text = tokenizer.apply_chat_template(req["messages"], tokenize=False, add_generation_prompt=False, enable_thinking=False)
    else:
        text = str(req.get("text") or "")
    if not text.strip():
        raise BadRequest("text or messages is required")
    ids = tokenizer.encode(text)[:MAX_TRACE_TOKENS]
    neurons = req.get("neurons") or []
    dirs = req.get("directions") or []
    k = max(0, min(int(req.get("top") or 0), 8))
    lens = max(0, min(int(req.get("lens") or 0), 5))
    look = req.get("attention")
    if look is not None:
        if not isinstance(look, dict) or not 0 <= int(look.get("layer", -1)) < len(layers):
            raise BadRequest(f"attention is {{layer, head}}, with layer below {len(layers)}")
    if len(neurons) > 16 or len(dirs) > 4:
        raise BadRequest("at most 16 neurons and 4 directions")
    for n in neurons:
        if not (0 <= int(n.get("layer", -1)) < len(layers) and 0 <= int(n.get("neuron", -1)) < width):
            raise BadRequest(f"no neuron {n.get('layer')}/{n.get('neuron')}")
    feats = req.get("features") or []
    if not isinstance(feats, list) or len(feats) > 16:
        raise BadRequest("features is a list of at most 16 {layer, feature}")
    feats = features_of(feats)
    strongest = req.get("top_features")
    if strongest is not None:
        if not isinstance(strongest, dict):
            raise BadRequest("top_features is {layer, top}")
        f_layer = default_feature_layer() if strongest.get("layer") is None else int(strongest["layer"])
        f_top = max(1, min(int(strongest.get("top") or 5), 16))
        features_at(f_layer)
    # Directions first: working one out runs the model, which would
    # overwrite what this pass records.
    vecs = []
    for d in dirs:
        layer = int(d.get("layer", -1))
        if not 0 <= layer < len(layers):
            raise BadRequest(f"no layer {layer}")
        vecs.append((d, layer, direction(d.get("target"), d.get("others"))[layer]))
    set_pins(req.get("steer"))  # trace what a steered model sees, if asked
    hooks.record, hooks.streams = {}, {}
    hooks.look = int(look["layer"]) if look is not None else None
    try:
        t0 = time.time()
        model(mx.array(ids)[None])
        out = {"object": "interp.trace", "model": NAME, "layers": len(layers), "neurons_per_layer": width,
               "tokens": [tokenizer.decode([i]) for i in ids]}
        out["neurons"] = [{"layer": int(n["layer"]), "neuron": int(n["neuron"]),
                           "values": [round(v, 3) for v in hooks.record[int(n["layer"])][0, :, int(n["neuron"])].astype(mx.float32).tolist()]}
                          for n in neurons]
        if feats:
            out["features"] = []
            for layer, f in feats:
                v = features_at(layer).encode(hooks.streams[layer][0, 1:])[:, f]  # not the first token (see scan)
                out["features"].append({"layer": layer, "feature": f, "values": [0.0] + [round(x, 3) for x in v.tolist()]})
        if strongest is not None:
            a = features_at(f_layer).encode(hooks.streams[f_layer][0]).at[0].multiply(0)
            idx = mx.argsort(-a, axis=-1)[:, :f_top]
            val = mx.take_along_axis(a, idx, axis=-1)
            # top_features[token] = [[feature, value], ...], strongest first; silent ones left out
            out["top_features"] = {"layer": f_layer, "tokens": [[[i, round(v, 2)] for i, v in zip(ri, rv) if v > 0]
                                                                for ri, rv in zip(idx.tolist(), val.tolist())]}
        out["directions"] = []
        for d, layer, v in vecs:
            # Cosine between each token's residual stream and the direction.
            r = hooks.streams[layer][0].astype(mx.float32)
            cos = (r @ v) / (mx.linalg.norm(r, axis=-1) * mx.linalg.norm(v) + 1e-6)
            out["directions"].append({"layer": layer, "target": d.get("target"), "values": [round(x, 4) for x in cos.tolist()]})
        if req.get("map") or req.get("map_token") is not None:
            out.update(neuron_map(len(ids), req.get("map_token")))
        if lens:
            out["lens"] = logit_lens(lens)
        if look is not None:
            out["attention"] = attention_of(look)
        if k:
            top = []
            for layer in range(len(layers)):
                h = hooks.record[layer][0].astype(mx.float32)
                idx = mx.argpartition(-h, kth=k - 1, axis=-1)[:, :k]
                val = mx.take_along_axis(h, idx, axis=-1)
                order = mx.argsort(-val, axis=-1)
                idx, val = mx.take_along_axis(idx, order, axis=-1), mx.take_along_axis(val, order, axis=-1)
                top.append([[[i, round(v, 2)] for i, v in zip(ri, rv)] for ri, rv in zip(idx.tolist(), val.tolist())])
            out["top"] = top  # top[layer][token] = [[neuron, value], ...], strongest first
        out["usage"] = {"prompt_tokens": len(ids), "completion_tokens": 0, "total_tokens": len(ids)}
        out["seconds"] = round(time.time() - t0, 2)
        job.out.put(("result", out))
    finally:
        hooks.record, hooks.streams = None, None
        unpin()
        hooks.look, hooks.looked = None, None


def logit_lens(k):
    """What the model would say next if each layer were its last: every
    layer's residual stream through the final norm and the head.
    lens[layer][token] = [[text, probability], ...], likeliest first."""
    rows = []
    for layer in range(len(layers)):
        p = mx.softmax(head(model.model.norm(hooks.streams[layer][0])).astype(mx.float32), axis=-1)
        idx = mx.argpartition(-p, kth=k - 1, axis=-1)[:, :k]
        val = mx.take_along_axis(p, idx, axis=-1)
        order = mx.argsort(-val, axis=-1)
        idx, val = mx.take_along_axis(idx, order, axis=-1), mx.take_along_axis(val, order, axis=-1)
        rows.append([[[decoded(i), round(v, 4)] for i, v in zip(ri, rv)] for ri, rv in zip(idx.tolist(), val.tolist())])
    return rows


def attention_of(look):
    """One layer's attention, one head or the heads' mean, as bytes:
    row q is where token q looked, 255 being all of its attention."""
    a = hooks.looked[0]  # heads × T × T
    h = look.get("head")
    if h is None or int(h) < 0:
        a, h = a.mean(axis=0), -1
    elif int(h) < a.shape[0]:
        a, h = a[int(h)], int(h)
    else:
        raise BadRequest(f"no head {h}: layer {look['layer']} has {a.shape[0]}")
    q = mx.clip(mx.round(a * 255), 0, 255).astype(mx.uint8)
    return {"layer": int(look["layer"]), "head": h, "heads": hooks.looked.shape[1], "shape": list(q.shape),
            "data": base64.b64encode(np.array(q).tobytes()).decode()}


def neuron_map(n_tokens, token=None):
    """Every MLP neuron in the model, as how hard it fired (0–255), for
    drawing. Each layer is scaled to its own typical activity, since later
    layers fire far harder. For all tokens, neighbouring neurons are pooled
    (their strongest) to keep the answer under MAP_BYTES; for one token,
    every neuron is there; "max" gives every neuron's peak over the text."""
    h = mx.abs(mx.stack([hooks.record[i][0] for i in range(len(layers))]).astype(mx.float32))  # layers × tokens × neurons
    scale = h.mean(axis=(1, 2), keepdims=True) * 12 + 1e-6
    q = mx.clip(h / scale * 255, 0, 255)
    out = {"map_scale": [round(x, 4) for x in scale.reshape(-1).tolist()]}
    if token == "max":  # each neuron at its strongest anywhere in the text
        one = q.max(axis=1).astype(mx.uint8)
        out["map_token"] = {"token": "max", "shape": [len(layers), width], "data": base64.b64encode(np.array(one).tobytes()).decode()}
        return out
    if token is not None:
        t = int(token)
        if not 0 <= t < n_tokens:
            raise BadRequest(f"no token {t}: there are {n_tokens}")
        one = q[:, t, :].astype(mx.uint8)
        out["map_token"] = {"token": t, "shape": [len(layers), width], "data": base64.b64encode(np.array(one).tobytes()).decode()}
        return out
    pool = 1
    while n_tokens * len(layers) * width // pool > MAP_BYTES and pool < width:
        pool *= 2
    pooled = q.reshape(len(layers), n_tokens, width // pool, pool).max(axis=-1).transpose(1, 0, 2).astype(mx.uint8)
    out["map"] = {"shape": [n_tokens, len(layers), width // pool], "pool": pool, "data": base64.b64encode(np.array(pooled).tobytes()).decode()}
    return out


def neurons_of(v, most=16):
    if not isinstance(v, list) or not 1 <= len(v) <= most:
        raise BadRequest(f"neurons is a list of 1 to {most} {{layer, neuron}}")
    out = []
    for n in v:
        layer, neuron = int(n.get("layer", -1)), int(n.get("neuron", -1))
        if not (0 <= layer < len(layers) and 0 <= neuron < width):
            raise BadRequest(f"no neuron {n.get('layer')}/{n.get('neuron')}")
        out.append((layer, neuron))
    return out


def scan(job):
    """Runs many short texts through the model, a batch at a time, and
    keeps where each chosen neuron fired hardest. Rows are padded on the
    right, which the causal mask keeps from touching the real tokens."""
    req = job.body
    texts = req.get("texts")
    if not isinstance(texts, list) or not 1 <= len(texts) <= MAX_SCAN_TEXTS:
        raise BadRequest(f"texts is a list of 1 to {MAX_SCAN_TEXTS} strings")
    neurons = neurons_of(req.get("neurons")) if req.get("neurons") or not req.get("features") else []
    feats = req.get("features") or []
    if not isinstance(feats, list) or len(neurons) + len(feats) > 16:
        raise BadRequest("at most 16 neurons and features")
    feats = features_of(feats)
    top = max(1, min(int(req.get("top") or 12), 50))
    ids = [tokenizer.encode(str(t)[:2000])[:MAX_SCAN_TOKENS] for t in texts]
    pad = tokenizer.pad_token_id if tokenizer.pad_token_id is not None else tokenizer.eos_token_id
    order = sorted((i for i in range(len(ids)) if ids[i]), key=lambda i: len(ids[i]))
    values = {}  # text → units (neurons, then features) × tokens
    t0 = time.time()
    hooks.record, hooks.record_only = {}, {layer for layer, _ in neurons}
    hooks.streams = {} if feats else None
    try:
        for b in range(0, len(order), SCAN_BATCH):
            if job.cancelled:
                return
            rows = order[b:b + SCAN_BATCH]
            n = max(len(ids[i]) for i in rows)
            x = mx.array([ids[i] + [pad] * (n - len(ids[i])) for i in rows])
            hooks.record.clear()
            model.model(x)  # lazy: only what the units need is computed
            cols = [hooks.record[layer][:, :, neuron].astype(mx.float32) for layer, neuron in neurons]
            enc = {}
            for layer, f in feats:
                if layer not in enc:
                    # The first token is where attention rests, its stream huge;
                    # what features read there isn't about the text.
                    enc[layer] = features_at(layer).encode(hooks.streams[layer]).at[:, 0].multiply(0)
                cols.append(enc[layer][:, :, f])
            v = mx.stack(cols, axis=1)
            for r, i in enumerate(rows):
                values[i] = np.array(v[r, :, :len(ids[i])])
            job.out.put(("progress", len(values)))
    finally:
        hooks.record, hooks.record_only, hooks.streams = None, None, None
    out = []
    for j, ((layer, neuron), w) in enumerate(zip(neurons, writes(neurons))):
        peaks = sorted(((values[i][j].max(), i) for i in values), reverse=True)
        out.append({"layer": layer, "neuron": neuron, "says": w["says"],
                    "max": [round(float(values[i][j].max()), 3) if i in values else None for i in range(len(ids))],
                    "examples": [{"text": i, "peak": round(float(p), 3), "at": int(values[i][j].argmax()),
                                  "tokens": [decoded(t) for t in ids[i]],
                                  "values": [round(float(x), 3) for x in values[i][j]]} for p, i in peaks[:top]]})
    found = []
    said = says_of(mx.stack([features_at(l).W_dec[f] for l, f in feats])) if feats else []
    for j, ((layer, f), w) in enumerate(zip(feats, said), start=len(neurons)):
        peaks = sorted(((values[i][j].max(), i) for i in values), reverse=True)
        found.append({"layer": layer, "feature": f, "says": w["says"], "says_z": w["says_z"], "similar": similar(layer, f),
                      "max": [round(float(values[i][j].max()), 3) if i in values else None for i in range(len(ids))],
                      "examples": [{"text": i, "peak": round(float(p), 3), "at": int(values[i][j].argmax()),
                                    "tokens": [decoded(t) for t in ids[i]],
                                    "values": [round(float(x), 3) for x in values[i][j]]} for p, i in peaks[:top]]})
    tokens = sum(len(i) for i in ids)
    job.out.put(("result", {"object": "interp.scan", "model": NAME, "neurons": out, "features": found,
                            "usage": {"prompt_tokens": tokens, "completion_tokens": 0, "total_tokens": tokens},
                            "seconds": round(time.time() - t0, 2)}))


SITES = ("stream", "mlp", "attn")


def patch(job):
    """Causal tracing. The clean prompt gets an answer; the corrupted one,
    the same length with the subject changed, a different one. Then for
    each layer and token, the corrupted prompt runs again with just that
    one activation copied from the clean run: where that brings the clean
    answer back is where the model holds what it needed."""
    req = job.body
    site = req.get("site") or "stream"
    if site not in SITES:
        raise BadRequest(f"site is one of {', '.join(SITES)}")
    clean = str(req.get("clean") or "")
    ids = tokenizer.encode(clean)
    if not 2 <= len(ids) <= MAX_PATCH_TOKENS:
        raise BadRequest(f"clean is 2 to {MAX_PATCH_TOKENS} tokens; it's {len(ids)}")
    tries = req.get("corrupt")
    tries = [tries] if isinstance(tries, str) else tries
    if not isinstance(tries, list) or not tries or len(tries) > 100:
        raise BadRequest("corrupt is a prompt, or a list of up to 100 to try in turn")
    lengths = {}
    for c in tries:
        cids = tokenizer.encode(str(c))
        lengths[str(c)] = len(cids)
        if len(cids) == len(ids) and cids != ids:
            corrupt = str(c)
            break
    else:
        raise BadRequest(f"the corrupted prompt must have as many tokens as the clean one ({len(ids)}); "
                         + ", ".join(f"{c!r} has {n}" for c, n in list(lengths.items())[:5]))
    t0 = time.time()
    T = len(ids)
    hooks.capture = {}
    try:
        p_clean = mx.softmax(last_logits(mx.array(ids)[None])[0])
        saved = {layer: hooks.capture[(site, layer)] for layer in range(len(layers))}
        mx.eval(p_clean, saved)
    finally:
        hooks.capture = None
    if req.get("answer"):
        a = tokenizer.encode(str(req["answer"]))
        if not a:
            raise BadRequest("answer has no tokens")
        answer = a[0]
    else:
        answer = int(mx.argmax(p_clean).item())
    p_bad = mx.softmax(last_logits(mx.array(cids)[None])[0])
    bad = int(mx.argmax(p_bad).item())
    # One row per token, each with the clean activation at that token only.
    x = mx.array([cids] * T)
    mask = mx.eye(T, dtype=mx.bool_)[:, :, None]
    grid = []
    try:
        for layer in range(len(layers)):
            if job.cancelled:
                return
            hooks.patch = {(site, layer): (mask, mx.broadcast_to(saved[layer], (T, T, saved[layer].shape[-1])))}
            p = mx.softmax(last_logits(x), axis=-1)[:, answer]
            grid.append([round(v, 4) for v in p.tolist()])
            job.out.put(("progress", layer + 1))
    finally:
        hooks.patch = {}
    job.out.put(("result", {
        "object": "interp.patch", "model": NAME, "site": site, "clean": clean, "corrupt": corrupt,
        "tokens": [decoded(i) for i in ids], "corrupt_tokens": [decoded(i) for i in cids],
        "answer": {"token": decoded(answer), "clean": round(p_clean[answer].item(), 4), "corrupt": round(p_bad[answer].item(), 4)},
        "corrupt_answer": {"token": decoded(bad), "p": round(p_bad[bad].item(), 4)},
        "grid": grid,  # grid[layer][token]: the clean answer's probability with that one patch
        "usage": {"prompt_tokens": T * (len(layers) + 2), "completion_tokens": 0, "total_tokens": T * (len(layers) + 2)},
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
            {"chat": chat, "contrast": contrast, "trace": trace, "scan": scan, "patch": patch}[job.kind](job)
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
            return self.send_json(200, {"model": MODEL, "loaded": model is not None, "queued": jobs.qsize(),
                                        "features": {"repo": SAE, "layers": sae_layers()} if SAE else None})
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
        elif self.path.startswith("/v1/interp/") and self.path[11:] in ("contrast", "trace", "scan", "patch"):
            job = Job(self.path[11:], body)
        else:
            return self.error(404, "this worker serves /v1/chat/completions and /v1/interp/{contrast,trace,scan,patch}")
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
    if sys.argv[1:2] == ["--get-sae"]:  # --get-sae repo dir [layer ...]
        get_sae(sys.argv[2], sys.argv[3], [int(x) for x in sys.argv[4:]])
        sys.exit(0)
    threading.Thread(target=watch_stdin, daemon=True).start()
    threading.Thread(target=model_thread, daemon=True).start()
    server = ThreadingHTTPServer((HOST, int(PORT)), Handler)
    server.daemon_threads = True
    log(f"serving {MODEL} on {HOST}:{PORT}")
    server.serve_forever()
