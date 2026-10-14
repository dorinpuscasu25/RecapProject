"""Builds the configured backends once per process and tracks readiness."""

from __future__ import annotations

import logging
import threading
from dataclasses import dataclass, field

from .backends.base import EmbeddingBackend, EntityBackend, RerankBackend
from .config import Settings

log = logging.getLogger(__name__)


@dataclass
class Registry:
    settings: Settings
    embedder: EmbeddingBackend
    reranker: RerankBackend | None
    entities: EntityBackend | None
    errors: dict[str, str] = field(default_factory=dict)
    loaded: set[str] = field(default_factory=set)
    _lock: threading.Lock = field(default_factory=threading.Lock)

    def load_all(self) -> None:
        for kind, backend in (("embeddings", self.embedder), ("rerank", self.reranker), ("entities", self.entities)):
            if backend is None:
                continue
            try:
                backend.load()
                with self._lock:
                    self.loaded.add(kind)
                    self.errors.pop(kind, None)
            except Exception as exc:  # surfaced through /readyz; never hidden
                log.exception("failed to load %s backend", kind)
                with self._lock:
                    self.errors[kind] = f"{type(exc).__name__}: {exc}"

    def ready(self) -> bool:
        want = {"embeddings"} | ({"rerank"} if self.reranker else set()) | ({"entities"} if self.entities else set())
        return want <= self.loaded

    def resolve_embedding(self, name: str | None) -> bool:
        return name in (None, "", self.settings.embedding_model_alias, self.embedder.model)

    def resolve_rerank(self, name: str | None) -> bool:
        if self.reranker is None:
            return False
        return name in (None, "", self.settings.rerank_model_alias, self.reranker.model)


def build(settings: Settings) -> Registry:
    s = settings
    if s.embedding_backend == "hash":
        from .backends.hashing import HashEmbedding

        embedder: EmbeddingBackend = HashEmbedding(s.embedding_model, s.embedding_dims)
    elif s.embedding_backend == "sentence_transformers":
        from .backends.st_backend import STEmbedding

        embedder = STEmbedding(s.embedding_model, s.embedding_dims, s.cache_dir, s.embedding_batch_size)
    else:
        from .backends.fastembed_backend import FastEmbedEmbedding

        embedder = FastEmbedEmbedding(
            s.embedding_model, s.embedding_dims, s.cache_dir, s.embedding_batch_size, s.threads
        )

    reranker: RerankBackend | None
    if s.rerank_backend == "none":
        reranker = None
    elif s.rerank_backend == "embedding":
        from .backends.embedding_rerank import EmbeddingRerank

        reranker = EmbeddingRerank(embedder)
    elif s.rerank_backend == "sentence_transformers":
        from .backends.st_backend import STRerank

        reranker = STRerank(s.rerank_model, s.cache_dir)
    else:
        from .backends.fastembed_backend import FastEmbedRerank

        reranker = FastEmbedRerank(s.rerank_model, s.cache_dir, s.threads)

    entities: EntityBackend | None = None
    if s.entities_backend == "transformers":
        from .backends.st_backend import TransformersEntities

        entities = TransformersEntities(s.entities_model, s.cache_dir)
    return Registry(settings=s, embedder=embedder, reranker=reranker, entities=entities)
