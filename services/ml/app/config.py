"""Configuration (12-factor; every setting is an environment variable)."""

from __future__ import annotations

from functools import lru_cache
from typing import Literal

from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="ML_", env_file=None, extra="ignore")

    # Embeddings. BGE-M3 is multilingual (ro/ru/en among 100+ languages), 1024-dim dense vectors.
    embedding_backend: Literal["fastembed", "sentence_transformers", "hash"] = "fastembed"
    embedding_model: str = "BAAI/bge-m3"
    embedding_model_alias: str = "bge-m3"  # name clients use in requests; the real id is also accepted
    embedding_dims: int = 1024  # used by the hash backend and for validation of custom models
    embedding_batch_size: int = 16

    # Reranking (cross-encoder). "embedding" = cosine similarity on the embedding model (no extra download).
    rerank_backend: Literal["fastembed", "sentence_transformers", "embedding", "none"] = "fastembed"
    rerank_model: str = "jinaai/jina-reranker-v2-base-multilingual"
    rerank_model_alias: str = "multilingual-reranker"

    # Entity extraction experiment: a Hugging Face token-classification model (needs the torch extra).
    entities_backend: Literal["none", "transformers"] = "none"
    entities_model: str = "Davlan/xlm-roberta-base-ner-hrl"

    cache_dir: str = "/models"
    preload: bool = True  # load models at startup so /readyz reflects real readiness
    threads: int = Field(default=0, description="ONNX threads; 0 = runtime default")

    api_key: str = ""  # optional bearer token required on /v1/*
    max_inputs: int = 256
    max_chars: int = 32_000


@lru_cache
def get_settings() -> Settings:
    return Settings()
