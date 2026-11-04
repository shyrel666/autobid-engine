"""Pre-download the PaddleOCR models used by service.py into tools/paddleocr/models/.

Run once after installing the pinned requirements (see docs/LOCAL_RUNTIME.md 3.3). The service itself never
downloads anything: it loads the model directories written here and refuses to start when they are missing.

    tools\\paddleocr\\.venv\\Scripts\\python.exe tools\\paddleocr\\fetch_models.py
"""

import json
import os
import shutil
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
MODELS = HERE / "models"
CACHE = HERE / ".paddlex-cache"

DET = os.environ.get("PADDLEOCR_DET_MODEL", "PP-OCRv5_mobile_det")
REC = os.environ.get("PADDLEOCR_REC_MODEL", "PP-OCRv5_mobile_rec")


def main() -> int:
    os.environ["PADDLE_PDX_CACHE_HOME"] = str(CACHE)
    os.environ.setdefault("PADDLE_PDX_MODEL_SOURCE", "bos")
    from paddleocr import PaddleOCR  # noqa: imported after the cache location is set

    PaddleOCR(
        text_detection_model_name=DET,
        text_recognition_model_name=REC,
        use_doc_orientation_classify=False,
        use_doc_unwarping=False,
        use_textline_orientation=False,
        device="cpu",
    )
    MODELS.mkdir(exist_ok=True)
    manifest_path = MODELS / "manifest.json"
    manifest = {}
    if manifest_path.exists():  # keep models fetched earlier (e.g. both mobile and server recognition)
        manifest = json.loads(manifest_path.read_text(encoding="utf-8")).get("files", {})
    for name in (DET, REC):
        src = CACHE / "official_models" / name
        if not src.is_dir():
            print(f"model {name} was not downloaded to {src}", file=sys.stderr)
            return 1
        dst = MODELS / name
        if dst.exists():
            shutil.rmtree(dst)
        shutil.copytree(src, dst)
        manifest[name] = sorted(p.name for p in dst.iterdir())
    manifest_path.write_text(json.dumps({"files": manifest}, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps({"det": DET, "rec": REC, "dir": str(MODELS)}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
