"""ONNX Runtime backends via fastembed (CPU-friendly, no PyTorch)."""

from __future__ import annotations

import logging
import threading
from typing import Sequence

log = logging.getLogger(__name__)

# Models fastembed does not ship in its catalogue but that publish an ONNX export on Hugging Face.
_CUSTOM_EMBEDDINGS = {
    "BAAI/bge-m3": dict(
        pooling="CLS",
        normalization=True,
        dim=1024,
        model_file="onnx/model.onnx",
        additional_files=["onnx/model.onnx_data"],
        size_in_gb=2.27,
        description="BGE-M3 dense embeddings (multilingual, 8192 tokens)",
    ),
}


def _register_custom(model: str) -> None:
    from fastembed import TextEmbedding
    from fastembed.common.model_description import ModelSource, PoolingType

    spec = _CUSTOM_EMBEDDINGS.get(model)
    if spec is None:
        return
    known = {m["model"] for m in TextEmbedding.list_supported_models()}
    if model in known:
        return
    TextEmbedding.add_custom_model(
        model=model,
        pooling=PoolingType[spec["pooling"]],
        normalization=spec["normalization"],
        sources=ModelSource(hf=model),
        dim=spec["dim"],
        model_file=spec["model_file"],
        additional_files=spec["additional_files"],
        size_in_gb=spec["size_in_gb"],
        description=spec["description"],
    )


class FastEmbedEmbedding:
    name = "fastembed"

    def __init__(self, model: str, dims: int, cache_dir: str, batch_size: int, threads: int) -> None:
        self.model, self.dims, self.cache_dir, self.batch_size, self.threads = (
            model,
            dims,
            cache_dir,
            batch_size,
            threads,
        )
        self._impl = None
        self._lock = threading.Lock()

    def load(self) -> None:
        with self._lock:
            if self._impl is not None:
                return
            from fastembed import TextEmbedding

            _register_custom(self.model)
            log.info("loading embedding model %s (fastembed)", self.model)
            self._impl = TextEmbedding(model_name=self.model, cache_dir=self.cache_dir, threads=self.threads or None)
            probe = next(iter(self._impl.embed(["probe"])))
            self.dims = len(probe)

    def embed(self, texts: Sequence[str]) -> list[list[float]]:
        self.load()
        return [v.tolist() for v in self._impl.embed(list(texts), batch_size=self.batch_size)]


class FastEmbedRerank:
    name = "fastembed"

    def __init__(self, model: str, cache_dir: str, threads: int) -> None:
        self.model, self.cache_dir, self.threads = model, cache_dir, threads
        self._impl = None
        self._lock = threading.Lock()

    def load(self) -> None:
        with self._lock:
            if self._impl is not None:
                return
            from fastembed.rerank.cross_encoder import TextCrossEncoder

            log.info("loading rerank model %s (fastembed)", self.model)
            self._impl = TextCrossEncoder(model_name=self.model, cache_dir=self.cache_dir, threads=self.threads or None)

    def score(self, query: str, documents: Sequence[str]) -> list[float]:
        self.load()
        return [float(s) for s in self._impl.rerank(query, list(documents))]
