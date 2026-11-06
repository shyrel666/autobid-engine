"""CPU-only local semantic embedding and reranking service.

The service reads only the pinned files under ``models/``. It never downloads
or executes code from a model repository. See .s6/SPEC.md section 8.
"""

from __future__ import annotations

import argparse
import hashlib
import http.server
import ipaddress
import json
import math
import sys
import threading
from pathlib import Path
from typing import Any, Iterable
from urllib.parse import urlsplit


HERE = Path(__file__).resolve().parent
MODELS_DIR = HERE / "models"
LOCK_PATH = HERE / "models.lock.json"
MAX_REQUEST_BYTES = 32 << 20
MAX_INPUT_LENGTH = 512
SERVICE_VERSION = "autobid-local-models/1.0.0"


class APIError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status
        self.message = message


def error_body(message: str) -> dict[str, str]:
    return {"error": message, "error_type": "Validation"}


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def model_sha(path: Path) -> str:
    return "sha256:" + file_sha256(path)[:16]


def validate_host(host: str) -> str:
    try:
        address = ipaddress.ip_address(host)
    except ValueError as exc:
        raise ValueError("--host must be a loopback IP address") from exc
    if not address.is_loopback:
        raise ValueError("--host must be a loopback IP address")
    return str(address)


def read_lock() -> dict[str, Any]:
    try:
        lock = json.loads(LOCK_PATH.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise RuntimeError(f"cannot read {LOCK_PATH.name}: {type(exc).__name__}") from exc
    if lock.get("schema_version") != 1 or not isinstance(lock.get("models"), dict):
        raise RuntimeError("unsupported models.lock.json format")
    return lock


def verify_model_files(lock: dict[str, Any]) -> dict[str, dict[str, Path]]:
    verified: dict[str, dict[str, Path]] = {}
    for name in ("embedding", "reranker"):
        try:
            model = lock["models"][name]
            files = model["files"]
        except (KeyError, TypeError) as exc:
            raise RuntimeError(f"models.lock.json is missing {name}") from exc
        verified[name] = {}
        for item in files:
            rel = Path(item["path"])
            if rel.is_absolute() or ".." in rel.parts:
                raise RuntimeError(f"unsafe local model path for {name}")
            path = (MODELS_DIR / name / rel).resolve()
            if not path.is_relative_to((MODELS_DIR / name).resolve()):
                raise RuntimeError(f"unsafe local model path for {name}")
            expected = item.get("sha256")
            if not isinstance(expected, str) or len(expected) != 64:
                raise RuntimeError(f"{name}/{rel.as_posix()} has no verified SHA-256; run fetch_models.py")
            if not path.is_file():
                raise RuntimeError(f"missing model file {name}/{rel.as_posix()}; run fetch_models.py")
            actual = file_sha256(path)
            if actual.lower() != expected.lower():
                raise RuntimeError(f"SHA-256 mismatch for {name}/{rel.as_posix()}: expected {expected}, got {actual}")
            key = item.get("role")
            if key not in ("onnx", "tokenizer"):
                raise RuntimeError(f"invalid file role for {name}/{rel.as_posix()}")
            verified[name][key] = path
        if set(verified[name]) != {"onnx", "tokenizer"}:
            raise RuntimeError(f"{name} must have exactly one ONNX file and one tokenizer")
    return verified


def _ort_dtype(input_meta: Any, values: Any):
    import numpy as np

    if input_meta.type == "tensor(int32)":
        return np.asarray(values, dtype=np.int32)
    if input_meta.type == "tensor(int64)":
        return np.asarray(values, dtype=np.int64)
    raise RuntimeError(f"unsupported ONNX input type for {input_meta.name}: {input_meta.type}")


class RuntimeModel:
    def __init__(
        self,
        name: str,
        model_id: str,
        model_file: Path,
        tokenizer_file: Path,
        dtype: str,
        batch_size: int,
        threads: int,
    ):
        import onnxruntime as ort
        import numpy as np
        from tokenizers import Tokenizer

        if batch_size < 1:
            raise ValueError("batch size must be positive")
        if threads < 1:
            raise ValueError("threads must be positive")
        self.name = name
        self.model_id = model_id
        self.model_file = model_file
        self.dtype = dtype
        self.batch_size = batch_size
        self.lock = threading.Lock()
        self.tokenizer = Tokenizer.from_file(str(tokenizer_file))
        self.tokenizer.no_padding()
        self.tokenizer.no_truncation()

        pad_id = self.tokenizer.token_to_id("[PAD]")
        if pad_id is None:
            pad_id = self.tokenizer.token_to_id("<pad>")
        if pad_id is None:
            raise RuntimeError(f"{name} tokenizer does not define a padding token")
        self.pad_id = int(pad_id)

        options = ort.SessionOptions()
        options.intra_op_num_threads = threads
        options.inter_op_num_threads = 1
        options.execution_mode = ort.ExecutionMode.ORT_SEQUENTIAL
        options.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
        self.session = ort.InferenceSession(
            str(model_file),
            sess_options=options,
            providers=["CPUExecutionProvider"],
        )
        if self.session.get_providers()[0] != "CPUExecutionProvider":
            raise RuntimeError("model did not initialize with CPUExecutionProvider")
        self.inputs = {meta.name: meta for meta in self.session.get_inputs()}
        self.outputs = self.session.get_outputs()
        self.np = np
        self.ort_version = ort.__version__
        self.model_sha = model_sha(model_file)

    @property
    def info(self) -> dict[str, str | int]:
        return {
            "model_id": self.model_id,
            "model_sha": self.model_sha,
            "model_dtype": self.dtype,
            "max_input_length": MAX_INPUT_LENGTH,
            "version": SERVICE_VERSION,
            "engine": f"onnxruntime {self.ort_version}; CPUExecutionProvider",
        }

    def _make_feed(self, encodings: list[Any]) -> dict[str, Any]:
        max_len = max(len(e.ids) for e in encodings)
        size = len(encodings)
        ids = [[self.pad_id] * max_len for _ in range(size)]
        masks = [[0] * max_len for _ in range(size)]
        type_ids = [[0] * max_len for _ in range(size)]
        for row, encoding in enumerate(encodings):
            length = len(encoding.ids)
            ids[row][:length] = encoding.ids
            masks[row][:length] = encoding.attention_mask
            if encoding.type_ids:
                type_ids[row][:length] = encoding.type_ids

        feed: dict[str, Any] = {}
        for name, meta in self.inputs.items():
            normalized = name.lower()
            if normalized == "input_ids":
                values = ids
            elif normalized == "attention_mask":
                values = masks
            elif normalized == "token_type_ids":
                values = type_ids
            elif normalized == "position_ids":
                positions = list(range(max_len))
                values = [positions[:] for _ in range(size)]
            else:
                raise RuntimeError(f"unsupported ONNX input: {name}")
            feed[name] = _ort_dtype(meta, values)
        return feed

    def _run_sorted(self, encodings: list[Any]) -> list[Any]:
        indexed = sorted(enumerate(encodings), key=lambda pair: len(pair[1].ids))
        output: list[Any] = [None] * len(encodings)
        for start in range(0, len(indexed), self.batch_size):
            chunk = indexed[start : start + self.batch_size]
            batch_encodings = [encoding for _, encoding in chunk]
            results = self.session.run(None, self._make_feed(batch_encodings))
            for result_index, (original_index, _) in enumerate(chunk):
                output[original_index] = [result[result_index] for result in results]
        return output

    def embed(self, texts: list[str], truncate: bool) -> list[list[float]]:
        with self.lock:
            encodings = []
            if truncate:
                # The tokenizer's own truncation keeps the trailing [SEP]; Encoding.truncate would cut it off.
                self.tokenizer.enable_truncation(MAX_INPUT_LENGTH, direction="right")
            try:
                for text in texts:
                    encoding = self.tokenizer.encode(text, add_special_tokens=True)
                    if len(encoding.ids) > MAX_INPUT_LENGTH:
                        raise APIError(413, "input exceeds max_input_length")
                    encodings.append(encoding)
            finally:
                self.tokenizer.no_truncation()
            results = self._run_sorted(encodings)

        vectors: list[list[float]] = []
        for result_list in results:
            candidates = list(zip(self.outputs, result_list))
            hidden = next((value for meta, value in candidates if len(value.shape) == 2), None)
            if hidden is not None:
                values = self.np.asarray(hidden[0], dtype=self.np.float32)
            else:
                vector = next((value for meta, value in candidates if len(value.shape) == 1), None)
                if vector is None:
                    raise RuntimeError("embedding ONNX output has no hidden-state tensor")
                values = self.np.asarray(vector, dtype=self.np.float32)
            norm = float(self.np.linalg.norm(values))
            if not math.isfinite(norm) or norm == 0.0:
                raise RuntimeError("embedding model returned an invalid vector")
            values = values / norm
            vectors.append([float(v) for v in values])
        return vectors

    def rerank(self, query: str, texts: list[str], truncate: bool, raw_scores: bool) -> list[dict[str, int | float]]:
        with self.lock:
            query_only = self.tokenizer.encode(query, pair="", add_special_tokens=True)
            if len(query_only.ids) > MAX_INPUT_LENGTH:
                raise APIError(413, "query exceeds max_input_length and cannot be truncated")
            encodings = []
            if truncate:
                self.tokenizer.enable_truncation(MAX_INPUT_LENGTH, strategy="only_second", direction="right")
            else:
                self.tokenizer.no_truncation()
            try:
                for text in texts:
                    encoding = self.tokenizer.encode(query, pair=text, add_special_tokens=True)
                    if len(encoding.ids) > MAX_INPUT_LENGTH:
                        raise APIError(413, "input exceeds max_input_length")
                    encodings.append(encoding)
            finally:
                self.tokenizer.no_truncation()
            results = self._run_sorted(encodings)

        scores: list[float] = []
        for result_list in results:
            logits = None
            for meta, value in zip(self.outputs, result_list):
                if len(value.shape) == 1 or (len(value.shape) == 2 and value.shape[-1] in (1, 2)):
                    logits = value
                    break
            if logits is None:
                raise RuntimeError("reranker ONNX output has no classification logits")
            row = self.np.asarray(logits[0]).reshape(-1)
            score = float(row[-1])
            if not math.isfinite(score):
                raise RuntimeError("reranker returned an invalid score")
            if not raw_scores:
                if score >= 0:
                    score = 1.0 / (1.0 + math.exp(-score))
                else:
                    exp_score = math.exp(score)
                    score = exp_score / (1.0 + exp_score)
            scores.append(score)

        order = sorted(range(len(scores)), key=lambda idx: (-scores[idx], idx))
        return [{"index": idx, "score": scores[idx]} for idx in order]


class ModelBundle:
    def __init__(self, lock: dict[str, Any], batch_size: int, threads: int):
        verified = verify_model_files(lock)
        models = lock["models"]
        embedding = models["embedding"]
        reranker = models["reranker"]
        self.embedding = RuntimeModel(
            "embedding",
            embedding["model_id"],
            verified["embedding"]["onnx"],
            verified["embedding"]["tokenizer"],
            embedding["model_dtype"],
            batch_size,
            threads,
        )
        self.reranker = RuntimeModel(
            "reranker",
            reranker["model_id"],
            verified["reranker"]["onnx"],
            verified["reranker"]["tokenizer"],
            reranker["model_dtype"],
            batch_size,
            threads,
        )


class ServiceState:
    def __init__(self):
        self._lock = threading.Lock()
        self.status = "loading"
        self.bundle: ModelBundle | None = None
        self.error: str | None = None

    def set_ready(self, bundle: ModelBundle) -> None:
        with self._lock:
            self.bundle = bundle
            self.error = None
            self.status = "ready"

    def set_failed(self, exc: BaseException) -> None:
        with self._lock:
            self.bundle = None
            self.status = "failed"
            self.error = f"{type(exc).__name__}: model initialization failed"

    def snapshot(self) -> tuple[str, ModelBundle | None, str | None]:
        with self._lock:
            return self.status, self.bundle, self.error


class Handler(http.server.BaseHTTPRequestHandler):
    server_version = "autobid-local-models/1"

    def log_message(self, fmt: str, *args: Any) -> None:
        # Requests may contain bid text. Do not log request lines or bodies.
        return

    @property
    def state(self) -> ServiceState:
        return self.server.state  # type: ignore[attr-defined]

    def _reply(self, status: int, body: Any) -> None:
        raw = json.dumps(body, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(raw)

    def _fail(self, status: int, message: str) -> None:
        self._reply(status, error_body(message))

    def _ready_bundle(self) -> ModelBundle | None:
        status, bundle, error = self.state.snapshot()
        if status == "ready" and bundle is not None:
            return bundle
        if status == "failed":
            self._fail(503, error or "model initialization failed")
        else:
            self._fail(503, "models are still loading")
        return None

    def do_GET(self) -> None:
        path = urlsplit(self.path).path
        if path == "/health":
            status, _, error = self.state.snapshot()
            body: dict[str, str] = {"status": status}
            if status == "failed" and error:
                body["error"] = error
            return self._reply(200, body)
        endpoints = {
            "/embedding/info": "embedding",
            "/reranker/info": "reranker",
        }
        if path not in endpoints:
            return self._fail(404, "not found")
        bundle = self._ready_bundle()
        if bundle is None:
            return
        model = getattr(bundle, endpoints[path])
        return self._reply(200, model.info)

    def do_POST(self) -> None:
        path = urlsplit(self.path).path
        if path not in ("/embedding/embed", "/reranker/rerank"):
            return self._fail(404, "not found")
        bundle = self._ready_bundle()
        if bundle is None:
            return

        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            return self._fail(400, "invalid Content-Length")
        if length <= 0:
            return self._fail(400, "request body is empty")
        if length > MAX_REQUEST_BYTES:
            return self._fail(413, "request body exceeds 32 MiB")
        raw = self.rfile.read(length)
        if len(raw) != length:
            return self._fail(400, "incomplete request body")
        try:
            payload = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            return self._fail(400, "request body must be valid UTF-8 JSON")
        if not isinstance(payload, dict):
            return self._fail(422, "request must be a JSON object")

        truncate = payload.get("truncate", False)
        if not isinstance(truncate, bool):
            return self._fail(422, "truncate must be a boolean")

        if path == "/embedding/embed":
            inputs = payload.get("inputs")
            if isinstance(inputs, str):
                texts = [inputs]
            elif isinstance(inputs, list) and all(isinstance(v, str) for v in inputs):
                texts = inputs
            else:
                return self._fail(422, "inputs must be a string or an array of strings")
            if any(not text.strip() for text in texts):
                return self._fail(422, "inputs must not contain empty strings")
            try:
                result = bundle.embedding.embed(texts, truncate)
            except APIError as exc:
                return self._fail(exc.status, exc.message)
            except Exception as exc:
                return self._fail(500, f"embedding failed: {type(exc).__name__}")
            return self._reply(200, result)

        query = payload.get("query")
        texts = payload.get("texts")
        raw_scores = payload.get("raw_scores", False)
        if not isinstance(query, str) or not query.strip():
            return self._fail(422, "query must be a non-empty string")
        if not isinstance(texts, list) or not all(isinstance(v, str) for v in texts):
            return self._fail(422, "texts must be an array of strings")
        if any(not text.strip() for text in texts):
            return self._fail(422, "texts must not contain empty strings")
        if not isinstance(raw_scores, bool):
            return self._fail(422, "raw_scores must be a boolean")
        try:
            result = bundle.reranker.rerank(query, texts, truncate, raw_scores)
        except APIError as exc:
            return self._fail(exc.status, exc.message)
        except Exception as exc:
            return self._fail(500, f"reranking failed: {type(exc).__name__}")
        return self._reply(200, result)


class ThreadingHTTPServer(http.server.ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address: tuple[str, int], state: ServiceState):
        self.state = state
        super().__init__(address, Handler)


def create_server(host: str, port: int, state: ServiceState | None = None) -> ThreadingHTTPServer:
    safe_host = validate_host(host)
    return ThreadingHTTPServer((safe_host, port), state or ServiceState())


def _load_in_background(state: ServiceState, batch_size: int, threads: int) -> None:
    try:
        state.set_ready(ModelBundle(read_lock(), batch_size, threads))
    except BaseException as exc:
        state.set_failed(exc)
        print(f"model initialization failed: {type(exc).__name__}", file=sys.stderr, flush=True)


def main(argv: Iterable[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="CPU-only local embedding and reranker service")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=18766)
    parser.add_argument("--batch-size", type=int, default=16)
    parser.add_argument("--threads", type=int, default=6)
    args = parser.parse_args(argv)
    try:
        host = validate_host(args.host)
        if not 1 <= args.port <= 65535:
            raise ValueError("--port must be between 1 and 65535")
        if args.batch_size < 1 or args.threads < 1:
            raise ValueError("--batch-size and --threads must be positive")
    except ValueError as exc:
        parser.error(str(exc))

    state = ServiceState()
    loader = threading.Thread(
        target=_load_in_background,
        args=(state, args.batch_size, args.threads),
        name="model-loader",
        daemon=True,
    )
    loader.start()
    server = create_server(host, args.port, state)
    print(json.dumps({"listening": f"http://{host}:{args.port}", "status": "loading"}), flush=True)
    try:
        server.serve_forever(poll_interval=0.25)
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
