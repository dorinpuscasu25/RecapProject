"""PyTorch backends (optional extra: requirements-torch.txt)."""

from __future__ import annotations

import threading
from typing import Sequence


class STEmbedding:
    name = "sentence_transformers"

    def __init__(self, model: str, dims: int, cache_dir: str, batch_size: int) -> None:
        self.model, self.dims, self.cache_dir, self.batch_size = model, dims, cache_dir, batch_size
        self._impl = None
        self._lock = threading.Lock()

    def load(self) -> None:
        with self._lock:
            if self._impl is None:
                from sentence_transformers import SentenceTransformer

                self._impl = SentenceTransformer(self.model, cache_folder=self.cache_dir)
                self.dims = self._impl.get_sentence_embedding_dimension()

    def embed(self, texts: Sequence[str]) -> list[list[float]]:
        self.load()
        return self._impl.encode(list(texts), batch_size=self.batch_size, normalize_embeddings=True).tolist()


class STRerank:
    name = "sentence_transformers"

    def __init__(self, model: str, cache_dir: str) -> None:
        self.model, self.cache_dir = model, cache_dir
        self._impl = None
        self._lock = threading.Lock()

    def load(self) -> None:
        with self._lock:
            if self._impl is None:
                from sentence_transformers import CrossEncoder

                self._impl = CrossEncoder(self.model, cache_folder=self.cache_dir)

    def score(self, query: str, documents: Sequence[str]) -> list[float]:
        self.load()
        return [float(s) for s in self._impl.predict([(query, d) for d in documents])]


class TransformersEntities:
    name = "transformers"

    def __init__(self, model: str, cache_dir: str) -> None:
        self.model, self.cache_dir = model, cache_dir
        self._impl = None
        self._lock = threading.Lock()

    def load(self) -> None:
        with self._lock:
            if self._impl is None:
                from transformers import pipeline

                self._impl = pipeline(
                    "token-classification",
                    model=self.model,
                    aggregation_strategy="simple",
                    model_kwargs={"cache_dir": self.cache_dir},
                )

    def extract(self, text: str) -> list[dict]:
        self.load()
        return [
            {
                "text": e["word"],
                "label": e["entity_group"],
                "start": int(e["start"]),
                "end": int(e["end"]),
                "score": float(e["score"]),
            }
            for e in self._impl(text)
        ]
