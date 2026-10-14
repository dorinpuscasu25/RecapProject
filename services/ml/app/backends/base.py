"""Backend protocols. Backends are swapped by configuration; the API layer never imports a library directly."""

from __future__ import annotations

from typing import Protocol, Sequence


class EmbeddingBackend(Protocol):
    name: str  # backend kind, e.g. "fastembed"
    model: str  # real model id
    dims: int

    def load(self) -> None: ...

    def embed(self, texts: Sequence[str]) -> list[list[float]]: ...


class RerankBackend(Protocol):
    name: str
    model: str

    def load(self) -> None: ...

    def score(self, query: str, documents: Sequence[str]) -> list[float]: ...


class EntityBackend(Protocol):
    name: str
    model: str

    def load(self) -> None: ...

    def extract(self, text: str) -> list[dict]: ...
