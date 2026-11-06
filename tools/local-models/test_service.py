"""Offline contract tests; model behavior tests run when pinned files exist."""

from __future__ import annotations

import http.client
import json
import math
import threading
import unittest

import service


def model_files_present() -> bool:
    try:
        lock = service.read_lock()
        service.verify_model_files(lock)
        return True
    except Exception:
        return False


class ServerCase(unittest.TestCase):
    state: service.ServiceState
    server: service.ThreadingHTTPServer
    thread: threading.Thread

    def start_server(self, state: service.ServiceState) -> None:
        self.state = state
        self.server = service.create_server("127.0.0.1", 0, state)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def stop_server(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def request(self, method: str, path: str, payload=None):
        conn = http.client.HTTPConnection("127.0.0.1", self.server.server_port, timeout=120)
        body = None if payload is None else json.dumps(payload, ensure_ascii=False).encode("utf-8")
        headers = {"Content-Type": "application/json; charset=utf-8"} if body is not None else {}
        conn.request(method, path, body=body, headers=headers)
        response = conn.getresponse()
        raw = response.read()
        status = response.status
        conn.close()
        return status, json.loads(raw.decode("utf-8"))


class LocalServiceContractTests(ServerCase):
    def test_non_loopback_host_is_rejected(self):
        with self.assertRaises(ValueError):
            service.create_server("0.0.0.0", 0)

    def test_health_and_loading_returns_503_for_business_endpoints(self):
        self.start_server(service.ServiceState())
        try:
            status, body = self.request("GET", "/health")
            self.assertEqual(status, 200)
            self.assertEqual(body, {"status": "loading"})
            status, body = self.request("GET", "/embedding/info")
            self.assertEqual(status, 503)
            self.assertEqual(body["error_type"], "Validation")
            status, body = self.request("POST", "/embedding/embed", {"inputs": "文本"})
            self.assertEqual(status, 503)
        finally:
            self.stop_server()

    def test_unknown_path(self):
        self.start_server(service.ServiceState())
        try:
            status, _ = self.request("GET", "/unknown")
            self.assertEqual(status, 404)
        finally:
            self.stop_server()


@unittest.skipUnless(model_files_present(), "pinned models are not present")
class RealModelTests(ServerCase):
    @classmethod
    def setUpClass(cls):
        cls.bundle = service.ModelBundle(service.read_lock(), batch_size=16, threads=6)

    def setUp(self):
        state = service.ServiceState()
        state.set_ready(self.bundle)
        self.start_server(state)

    def tearDown(self):
        self.stop_server()

    def test_embedding_dimension_norm_and_semantic_similarity(self):
        related = "投标文件应由法定代表人或授权代表签字，并加盖投标人单位公章。"
        unrelated = "本项目施工周期为一百二十天，现场实行安全生产管理。"
        status, vectors = self.request(
            "POST",
            "/embedding/embed",
            {"inputs": ["投标文件必须加盖公章并由法定代表人签字。", related, unrelated]},
        )
        self.assertEqual(status, 200)
        self.assertEqual(len(vectors), 3)
        self.assertTrue(all(len(vector) == 512 for vector in vectors))
        for vector in vectors:
            self.assertAlmostEqual(math.sqrt(sum(value * value for value in vector)), 1.0, places=5)
        cosine_related = sum(a * b for a, b in zip(vectors[0], vectors[1]))
        cosine_unrelated = sum(a * b for a, b in zip(vectors[0], vectors[2]))
        self.assertGreater(cosine_related, cosine_unrelated)

    def test_reranker_orders_relevant_passage_first(self):
        status, ranked = self.request(
            "POST",
            "/reranker/rerank",
            {
                "query": "投标文件是否必须加盖单位公章？",
                "texts": [
                    "本项目工期为一百二十日历天，施工组织应符合安全规范。",
                    "投标文件须加盖投标人单位公章，并由法定代表人或授权代表签字。",
                ],
                "raw_scores": False,
                "truncate": False,
            },
        )
        self.assertEqual(status, 200)
        self.assertEqual(ranked[0]["index"], 1)
        self.assertGreaterEqual(ranked[0]["score"], ranked[1]["score"])

    def test_overlength_is_413_unless_truncation_is_enabled(self):
        long_text = "招标文件投标人应当提交单位公章和法定代表人签字。" * 100
        status, body = self.request("POST", "/embedding/embed", {"inputs": long_text, "truncate": False})
        self.assertEqual(status, 413)
        self.assertEqual(body["error_type"], "Validation")
        status, vector = self.request("POST", "/embedding/embed", {"inputs": long_text, "truncate": True})
        self.assertEqual(status, 200)
        self.assertEqual(len(vector), 1)
        self.assertEqual(len(vector[0]), 512)

    def test_info_model_sha_matches_file(self):
        status, info = self.request("GET", "/embedding/info")
        self.assertEqual(status, 200)
        model_file = service.MODELS_DIR / "embedding" / "model.onnx"
        self.assertEqual(info["model_sha"], service.model_sha(model_file))
        self.assertEqual(info["max_input_length"], 512)
        self.assertIn("onnxruntime", info["engine"])
        status, info = self.request("GET", "/reranker/info")
        self.assertEqual(status, 200)
        model_file = service.MODELS_DIR / "reranker" / "model_int8.onnx"
        self.assertEqual(info["model_sha"], service.model_sha(model_file))

    def test_empty_text_is_422_without_echoing_input(self):
        status, body = self.request("POST", "/embedding/embed", {"inputs": "  "})
        self.assertEqual(status, 422)
        self.assertNotIn("inputs", body)

    def test_reranker_only_truncates_candidate_and_rejects_long_query(self):
        long_text = "投标文件应当盖章并由法定代表人签字。" * 100
        payload = {
            "query": "投标文件是否需要单位公章？",
            "texts": [long_text],
            "truncate": False,
        }
        status, _ = self.request("POST", "/reranker/rerank", payload)
        self.assertEqual(status, 413)
        payload["truncate"] = True
        status, ranked = self.request("POST", "/reranker/rerank", payload)
        self.assertEqual(status, 200)
        self.assertEqual(ranked[0]["index"], 0)

        payload["query"] = "招标文件签字盖章要求。" * 200
        status, body = self.request("POST", "/reranker/rerank", payload)
        self.assertEqual(status, 413)
        self.assertNotIn("query", body)


if __name__ == "__main__":
    unittest.main(verbosity=2)
