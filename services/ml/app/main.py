"""FastAPI application.

Endpoints follow existing wire formats so the Go AI gateway needs no special client:
- POST /v1/embeddings   OpenAI-compatible (used by the `openai_compatible` provider kind)
- POST /v1/rerank       Cohere/Jina-compatible
- GET  /v1/models       OpenAI-compatible listing (with backend/kind/dims)
Experiments (not used by production paths): /v1/classify, /v1/entities.
"""

from __future__ import annotations

import logging
import threading
import time
from contextlib import asynccontextmanager
from typing import Any

from fastapi import Depends, FastAPI, Header, Request
from fastapi.responses import JSONResponse, PlainTextResponse
from prometheus_client import CONTENT_TYPE_LATEST, Counter, Histogram, generate_latest
from pydantic import BaseModel, Field

from .config import Settings, get_settings
from .experiments.classify import classify
from .registry import Registry, build

logging.basicConfig(level=logging.INFO, format='{"level":"%(levelname)s","logger":"%(name)s","msg":"%(message)s"}')
log = logging.getLogger("newsintel.ml")

REQUESTS = Counter("ml_requests_total", "Requests by endpoint and outcome", ["endpoint", "outcome"])
LATENCY = Histogram(
    "ml_request_seconds", "Inference latency", ["endpoint"], buckets=(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30)
)
ITEMS = Counter("ml_items_total", "Texts processed", ["endpoint"])


class EmbeddingsRequest(BaseModel):
    model: str | None = None
    input: str | list[str]
    encoding_format: str | None = "float"
    dimensions: int | None = None


class RerankRequest(BaseModel):
    model: str | None = None
    query: str
    documents: list[str | dict[str, Any]]
    top_n: int | None = Field(default=None, ge=1)
    return_documents: bool = False


class Label(BaseModel):
    id: str
    description: str


class ClassifyRequest(BaseModel):
    text: str
    labels: list[Label] = Field(min_length=1, max_length=200)


class EntitiesRequest(BaseModel):
    text: str


class APIError(Exception):
    def __init__(self, status: int, code: str, message: str) -> None:
        self.status, self.code, self.message = status, code, message


def create_app(settings: Settings | None = None, registry: Registry | None = None) -> FastAPI:
    settings = settings or get_settings()
    reg = registry or build(settings)

    @asynccontextmanager
    async def lifespan(_: FastAPI):
        if settings.preload:
            # Load in the background: the process answers /healthz immediately and /readyz turns 200
            # once models are in memory (first start downloads weights into the cache volume).
            threading.Thread(target=reg.load_all, name="model-loader", daemon=True).start()
        yield

    app = FastAPI(title="newsintel-ml", version="0.1.0", lifespan=lifespan)
    app.state.registry = reg

    @app.exception_handler(APIError)
    async def api_error(_: Request, exc: APIError) -> JSONResponse:  # OpenAI-style error envelope
        return JSONResponse(
            status_code=exc.status, content={"error": {"message": exc.message, "type": exc.code, "code": exc.code}}
        )

    def auth(authorization: str | None = Header(default=None)) -> None:
        if settings.api_key and authorization != f"Bearer {settings.api_key}":
            raise APIError(401, "invalid_api_key", "Missing or invalid bearer token.")

    def check_inputs(texts: list[str]) -> None:
        if not texts:
            raise APIError(422, "invalid_request", "At least one input is required.")
        if len(texts) > settings.max_inputs:
            raise APIError(413, "too_many_inputs", f"At most {settings.max_inputs} inputs per request.")
        if any(len(t) > settings.max_chars for t in texts):
            raise APIError(413, "input_too_long", f"Inputs are limited to {settings.max_chars} characters.")

    def require_loaded(kind: str) -> None:
        if kind not in reg.loaded and not settings.preload:
            reg.load_all()  # lazy mode: load on first use
        if kind not in reg.loaded:
            detail = reg.errors.get(kind, "model is still loading")
            raise APIError(503, "model_unavailable", f"{kind} backend not ready: {detail}")

    @app.get("/healthz")
    def healthz() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/readyz")
    def readyz() -> JSONResponse:
        ok = reg.ready()
        return JSONResponse(
            status_code=200 if ok else 503,
            content={"status": "ready" if ok else "not_ready", "loaded": sorted(reg.loaded), "errors": reg.errors},
        )

    @app.get("/metrics")
    def metrics() -> PlainTextResponse:
        return PlainTextResponse(generate_latest(), media_type=CONTENT_TYPE_LATEST)

    @app.get("/v1/models", dependencies=[Depends(auth)])
    def models() -> dict[str, Any]:
        data = [
            {
                "id": settings.embedding_model_alias,
                "object": "model",
                "owned_by": "local",
                "kind": "embedding",
                "model": reg.embedder.model,
                "backend": reg.embedder.name,
                "dims": reg.embedder.dims,
                "ready": "embeddings" in reg.loaded,
            }
        ]
        if reg.reranker:
            data.append(
                {
                    "id": settings.rerank_model_alias,
                    "object": "model",
                    "owned_by": "local",
                    "kind": "rerank",
                    "model": reg.reranker.model,
                    "backend": reg.reranker.name,
                    "ready": "rerank" in reg.loaded,
                }
            )
        if reg.entities:
            data.append(
                {
                    "id": "entities",
                    "object": "model",
                    "owned_by": "local",
                    "kind": "token_classification",
                    "model": reg.entities.model,
                    "backend": reg.entities.name,
                    "ready": "entities" in reg.loaded,
                }
            )
        return {"object": "list", "data": data}

    @app.post("/v1/embeddings", dependencies=[Depends(auth)])
    def embeddings(req: EmbeddingsRequest) -> dict[str, Any]:
        if not reg.resolve_embedding(req.model):
            raise APIError(404, "model_not_found", f"Model {req.model!r} is not served here.")
        if req.encoding_format not in (None, "float"):
            raise APIError(422, "invalid_request", "Only encoding_format=float is supported.")
        texts = [req.input] if isinstance(req.input, str) else req.input
        check_inputs(texts)
        require_loaded("embeddings")
        if req.dimensions is not None and req.dimensions != reg.embedder.dims:
            raise APIError(422, "invalid_request", f"This model produces {reg.embedder.dims} dimensions.")
        start = time.perf_counter()
        vecs = reg.embedder.embed(texts)
        LATENCY.labels("embeddings").observe(time.perf_counter() - start)
        ITEMS.labels("embeddings").inc(len(texts))
        REQUESTS.labels("embeddings", "ok").inc()
        tokens = sum(max(1, len(t) // 4) for t in texts)  # estimate; models here are billed at zero cost
        return {
            "object": "list",
            "model": settings.embedding_model_alias,
            "backend": reg.embedder.name,
            "data": [{"object": "embedding", "index": i, "embedding": v} for i, v in enumerate(vecs)],
            "usage": {"prompt_tokens": tokens, "total_tokens": tokens},
        }

    @app.post("/v1/rerank", dependencies=[Depends(auth)])
    def rerank(req: RerankRequest) -> dict[str, Any]:
        if not reg.resolve_rerank(req.model):
            raise APIError(404, "model_not_found", f"Rerank model {req.model!r} is not served here.")
        docs = [d if isinstance(d, str) else str(d.get("text", "")) for d in req.documents]
        check_inputs([req.query, *docs])
        require_loaded("rerank")
        start = time.perf_counter()
        scores = reg.reranker.score(req.query, docs)
        LATENCY.labels("rerank").observe(time.perf_counter() - start)
        ITEMS.labels("rerank").inc(len(docs))
        REQUESTS.labels("rerank", "ok").inc()
        order = sorted(range(len(docs)), key=lambda i: scores[i], reverse=True)
        if req.top_n:
            order = order[: req.top_n]
        results = []
        for i in order:
            r: dict[str, Any] = {"index": i, "relevance_score": scores[i]}
            if req.return_documents:
                r["document"] = {"text": docs[i]}
            results.append(r)
        return {
            "model": settings.rerank_model_alias,
            "backend": reg.reranker.name,
            "results": results,
            "usage": {"total_tokens": sum(max(1, len(t) // 4) for t in [req.query, *docs])},
        }

    @app.post("/v1/classify", dependencies=[Depends(auth)])
    def classify_endpoint(req: ClassifyRequest) -> dict[str, Any]:
        check_inputs([req.text, *[lb.description for lb in req.labels]])
        require_loaded("embeddings")
        scores = classify(reg.embedder, req.text, [(lb.id, lb.description) for lb in req.labels])
        REQUESTS.labels("classify", "ok").inc()
        return {
            "experimental": True,
            "model": settings.embedding_model_alias,
            "backend": reg.embedder.name,
            "scores": [{"id": i, "score": s} for i, s in scores],
        }

    @app.post("/v1/entities", dependencies=[Depends(auth)])
    def entities(req: EntitiesRequest) -> dict[str, Any]:
        if reg.entities is None:
            raise APIError(
                501, "not_configured", "Entity extraction experiment is disabled (ML_ENTITIES_BACKEND=none)."
            )
        check_inputs([req.text])
        require_loaded("entities")
        return {"experimental": True, "model": reg.entities.model, "entities": reg.entities.extract(req.text)}

    return app


app = create_app()
