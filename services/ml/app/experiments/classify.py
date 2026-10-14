"""Zero-shot semantic classification by embedding similarity.

An experiment for comparing against the Go classification route; the Go backend remains the only
place where category assignments are decided and stored.
"""

from __future__ import annotations

from ..backends.base import EmbeddingBackend
from ..backends.embedding_rerank import cosine


def classify(embedder: EmbeddingBackend, text: str, labels: list[tuple[str, str]]) -> list[tuple[str, float]]:
    vecs = embedder.embed([text, *[desc for _, desc in labels]])
    scores = [(lid, cosine(vecs[0], v)) for (lid, _), v in zip(labels, vecs[1:])]
    return sorted(scores, key=lambda x: x[1], reverse=True)
