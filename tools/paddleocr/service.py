"""AutoBid local OCR service (PaddleOCR), see docs/LOCAL_RUNTIME.md 3.3 and AGENT_TOOL_SKILLS_PLAN.md 6.

Protocol (ingest.HTTPOCR):
    GET  /ocr   -> {"engine", "version", "ready", "device"}; version pins paddle, paddleocr and the model files.
    POST /ocr   raw image bytes (Content-Type image/*), optional headers
                X-Autobid-Request-Id (echoed) and X-Autobid-Content-SHA256 (must match the body),
                -> {"request_id", "engine", "version", "input_sha256", "width", "height",
                    "lines": [{"text", "bbox": [x0, y0, x1, y1], "confidence"}], "elapsed_ms"}
Errors are JSON {"error": <code>, "message": <text>}: 400 bad_request / hash_mismatch, 401 unauthorized,
413 too_large, 415 unsupported_image, 500 ocr_failed, 503 not_ready.

The service listens on 127.0.0.1 only, loads the model directories written by fetch_models.py and never
downloads anything. One recognition runs at a time; callers queue (the Go client has its own timeout).

    tools\\paddleocr\\.venv\\Scripts\\python.exe tools\\paddleocr\\service.py --port 18765
"""

import argparse
import hashlib
import io
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
MODELS = HERE / "models"
MAX_BYTES = 32 << 20
MAX_PIXELS = 60_000_000

# Never contact model hosters while serving; models come from MODELS only.
os.environ.setdefault("PADDLE_PDX_DISABLE_MODEL_SOURCE_CHECK", "True")
os.environ.setdefault("PADDLE_PDX_CACHE_HOME", str(HERE / ".paddlex-cache"))


def model_digest(model_dir: Path) -> str:
    h = hashlib.sha256()
    for name in ("inference.json", "inference.pdiparams", "inference.yml"):
        p = model_dir / name
        if p.exists():
            h.update(name.encode())
            h.update(p.read_bytes())
    return h.hexdigest()[:12]


class Engine:
    def __init__(self, det: str, rec: str, device: str):
        det_dir, rec_dir = MODELS / det, MODELS / rec
        for d in (det_dir, rec_dir):
            if not (d / "inference.pdiparams").exists():
                raise SystemExit(f"model directory {d} is missing; run fetch_models.py first")
        import numpy  # noqa: F401  (fail fast when the environment is incomplete)
        import paddle
        import paddleocr
        from paddleocr import PaddleOCR

        self.ocr = PaddleOCR(
            text_detection_model_name=det,
            text_detection_model_dir=str(det_dir),
            text_recognition_model_name=rec,
            text_recognition_model_dir=str(rec_dir),
            use_doc_orientation_classify=False,
            use_doc_unwarping=False,
            use_textline_orientation=False,
            device=device,
            # paddle 3.3.1 on Windows fails in the oneDNN executor with
            # "ConvertPirAttribute2RuntimeAttribute not support"; plain CPU kernels are used instead.
            enable_mkldnn=False,
        )
        self.device = device
        self.version = (
            f"paddleocr {paddleocr.__version__}; paddle {paddle.__version__}; "
            f"{det}@{model_digest(det_dir)}; {rec}@{model_digest(rec_dir)}; {device}"
        )
        self.lock = threading.Lock()

    def recognize(self, data: bytes):
        import numpy as np
        from PIL import Image, UnidentifiedImageError

        try:
            img = Image.open(io.BytesIO(data))
            img.seek(0)
            if img.width * img.height > MAX_PIXELS:
                raise ValueError("too_large")
            rgb = img.convert("RGB")
        except UnidentifiedImageError as e:
            raise TypeError(str(e)) from e
        arr = np.asarray(rgb)[:, :, ::-1].copy()  # BGR, as PaddleOCR expects
        with self.lock:
            results = self.ocr.predict(arr)
        lines = []
        for res in results:
            texts, scores, boxes = res["rec_texts"], res["rec_scores"], res["rec_boxes"]
            for text, score, box in zip(texts, scores, boxes):
                text = str(text).strip()
                if not text:
                    continue
                x0, y0, x1, y1 = (float(v) for v in box)
                lines.append({"text": text, "bbox": [x0, y0, x1, y1], "confidence": round(float(score), 4)})
        return rgb.width, rgb.height, lines


class Handler(BaseHTTPRequestHandler):
    engine: Engine = None
    token = ""
    server_version = "autobid-paddleocr/1"

    def log_message(self, fmt, *args):  # keep stdout for errors only
        pass

    def reply(self, status: int, body: dict):
        raw = json.dumps(body, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def fail(self, status: int, code: str, message: str):
        self.reply(status, {"error": code, "message": message})

    def authorized(self) -> bool:
        if not self.token:
            return True
        return self.headers.get("Authorization", "") == "Bearer " + self.token

    def do_GET(self):
        if self.path.split("?")[0] not in ("/ocr", "/health"):
            return self.fail(404, "not_found", "use /ocr")
        if not self.authorized():
            return self.fail(401, "unauthorized", "missing or wrong bearer token")
        self.reply(200, {"engine": "paddleocr", "version": self.engine.version, "ready": True, "device": self.engine.device})

    def do_POST(self):
        if self.path.split("?")[0] != "/ocr":
            return self.fail(404, "not_found", "use /ocr")
        if not self.authorized():
            return self.fail(401, "unauthorized", "missing or wrong bearer token")
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            return self.fail(400, "bad_request", "invalid Content-Length")
        if length <= 0:
            return self.fail(400, "bad_request", "empty body")
        if length > MAX_BYTES:
            return self.fail(413, "too_large", f"image larger than {MAX_BYTES} bytes")
        data = self.rfile.read(length)
        digest = hashlib.sha256(data).hexdigest()
        want = self.headers.get("X-Autobid-Content-SHA256", "").strip().lower()
        if want and want != digest:
            return self.fail(400, "hash_mismatch", "body does not match X-Autobid-Content-SHA256")
        request_id = self.headers.get("X-Autobid-Request-Id", "")[:128]
        start = time.monotonic()
        try:
            width, height, lines = self.engine.recognize(data)
        except TypeError as e:
            return self.fail(415, "unsupported_image", str(e)[:200])
        except ValueError as e:
            if str(e) == "too_large":
                return self.fail(413, "too_large", f"image has more than {MAX_PIXELS} pixels")
            return self.fail(500, "ocr_failed", str(e)[:200])
        except Exception as e:  # recognition failure is reported, never turned into an empty page
            return self.fail(500, "ocr_failed", f"{type(e).__name__}: {str(e)[:200]}")
        self.reply(200, {
            "request_id": request_id, "engine": "paddleocr", "version": self.engine.version, "input_sha256": digest,
            "width": width, "height": height, "lines": lines, "elapsed_ms": int((time.monotonic() - start) * 1000),
        })


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=int(os.environ.get("PADDLEOCR_PORT", "18765")))
    ap.add_argument("--det", default=os.environ.get("PADDLEOCR_DET_MODEL", "PP-OCRv5_mobile_det"))
    ap.add_argument("--rec", default=os.environ.get("PADDLEOCR_REC_MODEL", "PP-OCRv5_mobile_rec"))
    ap.add_argument("--device", default=os.environ.get("PADDLEOCR_DEVICE", "cpu"))
    args = ap.parse_args()
    Handler.engine = Engine(args.det, args.rec, args.device)
    Handler.token = os.environ.get("AUTOBID_OCR_TOKEN", "")
    srv = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    srv.daemon_threads = True
    print(json.dumps({"listening": f"http://127.0.0.1:{args.port}/ocr", "version": Handler.engine.version}, ensure_ascii=False), flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
