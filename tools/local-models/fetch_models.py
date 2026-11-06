"""Download only the model files pinned by models.lock.json and verify SHA-256."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


HERE = Path(__file__).resolve().parent
MODELS_DIR = HERE / "models"
LOCK_PATH = HERE / "models.lock.json"
CHUNK_SIZE = 1024 * 1024


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(CHUNK_SIZE), b""):
            digest.update(block)
    return digest.hexdigest()


def read_lock() -> dict[str, Any]:
    lock = json.loads(LOCK_PATH.read_text(encoding="utf-8"))
    if lock.get("schema_version") != 1 or not isinstance(lock.get("models"), dict):
        raise ValueError("unsupported models.lock.json format")
    return lock


def write_lock(lock: dict[str, Any]) -> None:
    temporary = LOCK_PATH.with_suffix(".json.tmp")
    temporary.write_text(json.dumps(lock, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    os.replace(temporary, LOCK_PATH)


def download_file(url: str, target: Path) -> None:
    """Resume into target.part. A server that ignores Range restarts safely."""
    target.parent.mkdir(parents=True, exist_ok=True)
    partial = target.with_name(target.name + ".part")
    offset = partial.stat().st_size if partial.exists() else 0
    headers = {"User-Agent": "autobid-local-models/1.0", "Accept-Encoding": "identity"}
    if offset:
        headers["Range"] = f"bytes={offset}-"
    request = urllib.request.Request(url, headers=headers)
    try:
        response = urllib.request.urlopen(request, timeout=90)
    except urllib.error.HTTPError as exc:
        if offset and exc.code == 416:
            partial.unlink(missing_ok=True)
            return download_file(url, target)
        raise
    with response:
        status = getattr(response, "status", response.getcode())
        append = offset > 0 and status == 206
        if not append:
            offset = 0
        mode = "ab" if append else "wb"
        with partial.open(mode) as destination:
            while True:
                block = response.read(CHUNK_SIZE)
                if not block:
                    break
                destination.write(block)
            destination.flush()
            os.fsync(destination.fileno())


def fetch(mirror: str) -> dict[str, str]:
    lock = read_lock()
    measured: dict[str, str] = {}
    for name in ("embedding", "reranker"):
        model = lock["models"][name]
        repo = model["model_id"]
        revision = model["revision"]
        for item in model["files"]:
            rel = Path(item["path"])
            if rel.is_absolute() or ".." in rel.parts:
                raise ValueError(f"unsafe target path in lock: {rel}")
            target = MODELS_DIR / name / rel
            expected = item.get("sha256")
            if target.exists() and isinstance(expected, str):
                actual = sha256_file(target)
                if actual.lower() == expected.lower():
                    measured[f"{name}/{rel.as_posix()}"] = actual
                    print(f"verified existing {name}/{rel.as_posix()} sha256={actual}", flush=True)
                    continue
                target.unlink()
                raise RuntimeError(
                    f"SHA-256 mismatch for {name}/{rel.as_posix()}: expected {expected}, got {actual}; file removed"
                )

            url = f"{mirror.rstrip('/')}/{repo}/resolve/{revision}/{item['repo_path']}"
            print(f"downloading {repo}@{revision}:{item['repo_path']}", flush=True)
            # Preserve .part on transport errors so the next invocation can
            # resume with Range. Hash mismatches below still delete the bytes.
            download_file(url, target)
            actual = sha256_file(target.with_name(target.name + ".part"))

            if isinstance(expected, str) and actual.lower() != expected.lower():
                target.with_name(target.name + ".part").unlink(missing_ok=True)
                raise RuntimeError(
                    f"SHA-256 mismatch for {name}/{rel.as_posix()}: expected {expected}, got {actual}; partial file removed"
                )

            os.replace(target.with_name(target.name + ".part"), target)
            item["sha256"] = actual
            measured[f"{name}/{rel.as_posix()}"] = actual
            write_lock(lock)
            print(f"verified {name}/{rel.as_posix()} sha256={actual}", flush=True)

    env_path = MODELS_DIR / "env.bat"
    env_text = (
        "@echo off\r\n"
        f'set "AUTOBID_EMBEDDING_REVISION={model_sha_from_lock(lock, "embedding")}"\r\n'
        f'set "AUTOBID_RERANKER_REVISION={model_sha_from_lock(lock, "reranker")}"\r\n'
    )
    with env_path.open("w", encoding="ascii", newline="") as output:
        output.write(env_text)
    return measured


def model_sha_from_lock(lock: dict[str, Any], name: str) -> str:
    model = lock["models"][name]
    onnx = next(item for item in model["files"] if item["role"] == "onnx")
    if not isinstance(onnx.get("sha256"), str):
        raise RuntimeError(f"{name} ONNX SHA-256 is missing")
    return "sha256:" + onnx["sha256"][:16]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mirror", default="https://huggingface.co")
    args = parser.parse_args()
    if not args.mirror.startswith("https://"):
        parser.error("--mirror must use HTTPS")
    try:
        result = fetch(args.mirror)
    except Exception as exc:
        print(f"model fetch failed: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 1
    print(json.dumps({"files": result}, ensure_ascii=False, indent=2))
    print(f"wrote {MODELS_DIR / 'env.bat'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
