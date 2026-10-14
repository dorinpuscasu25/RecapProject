"""Retrieval evaluation for embedding/rerank models.

    python -m app.evaluate --dataset ../../fixtures/eval/retrieval.json [--url http://localhost:8000]

Without --url it evaluates the in-process configured backends; with --url it evaluates any
OpenAI-compatible embeddings endpoint (this service, vLLM, OpenAI…), so models can be compared
before switching the Go `embeddings` route. Reports recall@k, MRR and nDCG@10.
"""

from __future__ import annotations

import argparse
import json
import math
import sys
from typing import Callable, Sequence

from .backends.embedding_rerank import cosine


def metrics(rankings: list[list[str]], relevant: list[set[str]], ks=(1, 3, 5, 10)) -> dict[str, float]:
    n = len(rankings)
    out: dict[str, float] = {}
    for k in ks:
        out[f"recall@{k}"] = sum(len(set(r[:k]) & rel) / len(rel) for r, rel in zip(rankings, relevant)) / n
    out["mrr"] = (
        sum(next((1 / (i + 1) for i, d in enumerate(r) if d in rel), 0.0) for r, rel in zip(rankings, relevant)) / n
    )

    def ndcg(r: list[str], rel: set[str]) -> float:
        dcg = sum(1 / math.log2(i + 2) for i, d in enumerate(r[:10]) if d in rel)
        idcg = sum(1 / math.log2(i + 2) for i in range(min(10, len(rel))))
        return dcg / idcg if idcg else 0.0

    out["ndcg@10"] = sum(ndcg(r, rel) for r, rel in zip(rankings, relevant)) / n
    return out


def evaluate(embed: Callable[[Sequence[str]], list[list[float]]], dataset: dict) -> dict[str, float]:
    corpus = dataset["corpus"]
    ids = [d["id"] for d in corpus]
    doc_vecs = embed([d["text"] for d in corpus])
    rankings, relevant = [], []
    for q in dataset["queries"]:
        qv = embed([q["query"]])[0]
        order = sorted(range(len(ids)), key=lambda i: cosine(qv, doc_vecs[i]), reverse=True)
        rankings.append([ids[i] for i in order])
        relevant.append(set(q["relevant"]))
    return metrics(rankings, relevant)


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--dataset", required=True)
    p.add_argument("--url", help="OpenAI-compatible base URL (…/v1 is appended if missing)")
    p.add_argument("--model", default=None)
    p.add_argument("--api-key", default="")
    p.add_argument("--min-recall5", type=float, default=0.0, help="exit 3 if recall@5 is below this gate")
    a = p.parse_args(argv)
    with open(a.dataset, encoding="utf-8") as f:
        dataset = json.load(f)
    if a.url:
        import httpx

        base = a.url.rstrip("/")
        base = base if base.endswith("/v1") else base + "/v1"
        headers = {"Authorization": f"Bearer {a.api_key}"} if a.api_key else {}

        def embed(texts: Sequence[str]) -> list[list[float]]:
            r = httpx.post(
                f"{base}/embeddings", json={"model": a.model, "input": list(texts)}, headers=headers, timeout=300
            )
            r.raise_for_status()
            return [d["embedding"] for d in sorted(r.json()["data"], key=lambda d: d["index"])]

        label = f"{base} model={a.model}"
    else:
        from .config import get_settings
        from .registry import build

        reg = build(get_settings())
        reg.embedder.load()
        embed = reg.embedder.embed
        label = f"{reg.embedder.name}:{reg.embedder.model}"
    m = evaluate(embed, dataset)
    print(
        json.dumps(
            {"model": label, "queries": len(dataset["queries"]), "corpus": len(dataset["corpus"]), "metrics": m},
            indent=2,
        )
    )
    return 3 if m["recall@5"] < a.min_recall5 else 0


if __name__ == "__main__":
    sys.exit(main())
