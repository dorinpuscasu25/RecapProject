"""Deterministic feature-hashing embeddings.

For tests and offline development only: it captures lexical overlap, not meaning. The backend
name ("hash") is reported in /v1/models and every response so it cannot be mistaken for a model.
"""

from __future__ import annotations

import hashlib
import math
import re
import unicodedata
from typing import Sequence

_TOKEN = re.compile(r"\w+", re.UNICODE)


def _tokens(text: str) -> list[str]:
    text = unicodedata.normalize("NFKD", text.lower())
    text = "".join(c for c in text if not unicodedata.combining(c))
    return [t[:6] for t in _TOKEN.findall(text) if len(t) > 2]  # crude prefix stemming


class HashEmbedding:
    name = "hash"

    def __init__(self, model: str, dims: int) -> None:
        self.model = model
        self.dims = dims

    def load(self) -> None:
        return None

    def embed(self, texts: Sequence[str]) -> list[list[float]]:
        out = []
        for text in texts:
            v = [0.0] * self.dims
            for tok in _tokens(text):
                h = int.from_bytes(hashlib.blake2b(tok.encode(), digest_size=8).digest(), "little")
                v[h % self.dims] += 1.0 if (h >> 63) == 0 else -1.0
            n = math.sqrt(sum(x * x for x in v)) or 1.0
            out.append([x / n for x in v])
        return out
