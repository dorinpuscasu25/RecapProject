"""Real-model checks. Opt-in: ML_TEST_REAL_MODELS=1 (downloads weights from Hugging Face)."""

from __future__ import annotations

import os

import pytest

from app.backends.embedding_rerank import cosine
from app.config import Settings
from app.registry import build

pytestmark = pytest.mark.skipif(
    os.getenv("ML_TEST_REAL_MODELS") != "1", reason="set ML_TEST_REAL_MODELS=1 to download and test real models"
)


def test_bge_m3_cross_lingual():
    s = Settings(cache_dir=os.getenv("ML_CACHE_DIR", "/tmp/ml-models"), rerank_backend="none")
    reg = build(s)
    reg.load_all()
    assert not reg.errors, reg.errors
    ro, ru, other = reg.embedder.embed(
        [
            "ANRE a majorat tariful la energia electrică cu 15%",
            "НАРЭ повысило тариф на электроэнергию на 15%",
            "Echipa națională a câștigat meciul de fotbal",
        ]
    )
    assert reg.embedder.dims == 1024
    assert cosine(ro, ru) > cosine(ro, other) + 0.1


def test_multilingual_reranker():
    s = Settings(cache_dir=os.getenv("ML_CACHE_DIR", "/tmp/ml-models"), embedding_backend="hash")
    reg = build(s)
    reg.load_all()
    assert not reg.errors, reg.errors
    scores = reg.reranker.score(
        "тариф на электроэнергию", ["Meci de fotbal", "ANRE a majorat tariful la energia electrică"]
    )
    assert scores[1] > scores[0]
