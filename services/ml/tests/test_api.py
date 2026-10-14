"""API tests with the deterministic hash backend (no model download). The fastembed/BGE-M3 path is
covered by tests/test_models.py, which runs only when ML_TEST_REAL_MODELS=1 and weights are reachable."""

from __future__ import annotations

import json
import pathlib

import pytest
from fastapi.testclient import TestClient

from app.config import Settings
from app.main import create_app
from app.registry import build

ROOT = pathlib.Path(__file__).resolve().parents[3]


def client(**kw) -> TestClient:
    s = Settings(embedding_backend="hash", embedding_dims=256, rerank_backend="embedding", preload=False, **kw)
    reg = build(s)
    reg.load_all()
    return TestClient(create_app(settings=s, registry=reg))


def test_health_ready_and_models():
    c = client()
    assert c.get("/healthz").json() == {"status": "ok"}
    r = c.get("/readyz")
    assert r.status_code == 200 and set(r.json()["loaded"]) == {"embeddings", "rerank"}
    models = c.get("/v1/models").json()["data"]
    emb = next(m for m in models if m["kind"] == "embedding")
    assert emb["backend"] == "hash" and emb["dims"] == 256 and emb["id"] == "bge-m3"


def test_embeddings_openai_shape_and_order():
    c = client()
    r = c.post("/v1/embeddings", json={"model": "bge-m3", "input": ["Tariful la energie", "Тариф на электроэнергию"]})
    assert r.status_code == 200
    body = r.json()
    assert body["object"] == "list" and [d["index"] for d in body["data"]] == [0, 1]
    assert all(len(d["embedding"]) == 256 for d in body["data"]) and body["usage"]["prompt_tokens"] > 0
    single = c.post("/v1/embeddings", json={"input": "Tariful la energie"}).json()["data"][0]["embedding"]
    assert single == body["data"][0]["embedding"], "deterministic"


def test_embeddings_errors():
    c = client(max_inputs=2, max_chars=10)
    assert c.post("/v1/embeddings", json={"model": "other", "input": "x"}).status_code == 404
    assert c.post("/v1/embeddings", json={"input": ["a", "b", "c"]}).status_code == 413
    assert c.post("/v1/embeddings", json={"input": "x" * 11}).status_code == 413
    assert c.post("/v1/embeddings", json={"input": []}).status_code == 422
    assert c.post("/v1/embeddings", json={"input": "x", "dimensions": 12}).status_code == 422
    err = c.post("/v1/embeddings", json={"model": "other", "input": "x"}).json()
    assert err["error"]["code"] == "model_not_found"


def test_rerank_cohere_shape():
    c = client()
    r = c.post(
        "/v1/rerank",
        json={
            "query": "tarif energie electrica",
            "top_n": 2,
            "return_documents": True,
            "documents": [
                "Meci de fotbal la Chișinău",
                "ANRE a aprobat tariful la energia electrică",
                {"text": "Recolta de struguri"},
            ],
        },
    )
    assert r.status_code == 200
    res = r.json()["results"]
    assert len(res) == 2 and res[0]["index"] == 1 and res[0]["relevance_score"] >= res[1]["relevance_score"]
    assert res[0]["document"]["text"].startswith("ANRE")


def test_auth_required_when_configured():
    c = client(api_key="secret")
    assert c.post("/v1/embeddings", json={"input": "x"}).status_code == 401
    assert c.post("/v1/embeddings", json={"input": "x"}, headers={"Authorization": "Bearer secret"}).status_code == 200
    assert c.get("/healthz").status_code == 200  # probes stay open


def test_not_ready_is_explicit():
    class Broken:
        name, model, dims = "broken", "broken", 8

        def load(self):
            raise RuntimeError("weights not reachable")

        def embed(self, texts):
            raise AssertionError("must not be called")

    s = Settings(embedding_backend="hash", rerank_backend="none", preload=False)
    reg = build(s)
    reg.embedder = Broken()
    reg.load_all()
    c = TestClient(create_app(settings=s, registry=reg))
    r = c.get("/readyz")
    assert r.status_code == 503 and "weights not reachable" in r.json()["errors"]["embeddings"]
    e = c.post("/v1/embeddings", json={"input": "x"})
    assert e.status_code == 503 and e.json()["error"]["code"] == "model_unavailable"


def test_classify_and_entities_experiments():
    c = client()
    r = c.post(
        "/v1/classify",
        json={
            "text": "ANRE a aprobat tariful la energia electrică",
            "labels": [
                {"id": "sport", "description": "fotbal meci campionat"},
                {"id": "energy", "description": "tariful energia electrică ANRE"},
            ],
        },
    )
    assert r.status_code == 200 and r.json()["scores"][0]["id"] == "energy" and r.json()["experimental"] is True
    assert c.post("/v1/entities", json={"text": "Maia Sandu"}).status_code == 501


def test_retrieval_evaluation_runs_on_dataset():
    from app.backends.hashing import HashEmbedding
    from app.evaluate import evaluate

    ds = json.loads((ROOT / "fixtures" / "eval" / "retrieval.json").read_text(encoding="utf-8"))
    m = evaluate(HashEmbedding("hash", 256).embed, ds)
    assert set(m) >= {"recall@1", "recall@5", "mrr", "ndcg@10"}
    assert 0 <= m["mrr"] <= 1


@pytest.mark.parametrize("value", [None, "", "bge-m3", "BAAI/bge-m3"])
def test_model_aliases(value):
    s = Settings(embedding_backend="hash", rerank_backend="none", preload=False)
    assert build(s).resolve_embedding(value)
