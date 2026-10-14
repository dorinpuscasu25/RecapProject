"""Reranking by cosine similarity on the configured embedding model (no extra model download)."""

from __future__ import annotations

from typing import Sequence

from .base import EmbeddingBackend


def cosine(a: Sequence[float], b: Sequence[float]) -> float:
    dot = sum(x * y for x, y in zip(a, b))
    na = sum(x * x for x in a) ** 0.5
    nb = sum(y * y for y in b) ** 0.5
    return dot / (na * nb) if na and nb else 0.0


class EmbeddingRerank:
    name = "embedding"

    def __init__(self, embedder: EmbeddingBackend) -> None:
        self.embedder = embedder
        self.model = embedder.model

    def load(self) -> None:
        self.embedder.load()

    def score(self, query: str, documents: Sequence[str]) -> list[float]:
        vecs = self.embedder.embed([query, *documents])
        return [cosine(vecs[0], v) for v in vecs[1:]]
