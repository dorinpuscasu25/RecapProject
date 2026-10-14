# ML service

Stateless FastAPI model server for the news-intelligence backend. The Go AI gateway reaches it as
the `local-ml` provider (OpenAI-compatible); it returns vectors and scores, never business decisions
(see ADR-021 in `docs/decision-log.md`).

| Endpoint | Format | Purpose |
|---|---|---|
| `POST /v1/embeddings` | OpenAI | Multilingual embeddings (default BGE-M3, 1024 dimensions, normalized) |
| `POST /v1/rerank` | Cohere/Jina | Cross-encoder reranking (default `jinaai/jina-reranker-v2-base-multilingual`) |
| `GET /v1/models` | OpenAI | Loaded models and aliases (`bge-m3`, `multilingual-reranker`) |
| `POST /v1/classify` | — | Experiment: embedding similarity between texts and label descriptions |
| `POST /v1/entities` | — | Experiment: transformers NER (`ML_ENTITIES_BACKEND=transformers`; 501 when off) |
| `GET /healthz`, `/readyz`, `/metrics` | — | Liveness, readiness (503 with load errors until models are ready), Prometheus |

Errors use the OpenAI error envelope. With `ML_API_KEY` set, model endpoints require
`Authorization: Bearer <key>`. Inputs are limited by `ML_MAX_INPUTS` and `ML_MAX_CHARS`.

## Backends

| Setting | Values |
|---|---|
| `ML_EMBEDDING_BACKEND` | `fastembed` (ONNX, default), `sentence_transformers` (needs `requirements-torch.txt`), `hash` (deterministic feature hashing: **tests and offline only**) |
| `ML_RERANK_BACKEND` | `fastembed` (default), `sentence_transformers`, `embedding` (cosine over the embedding backend), `none` |
| `ML_EMBEDDING_MODEL`, `ML_EMBEDDING_DIMS`, `ML_RERANK_MODEL` | Model ids; dimensions must match the model |
| `ML_CACHE_DIR` | Model cache (`/models` in the container, volume `mlmodels`) |
| `ML_PRELOAD`, `ML_THREADS`, `ML_EMBEDDING_BATCH_SIZE` | Loading and performance |

Changing the embedding model creates a different vector space: register it in the gateway and run
the embedding migration (`docs/ai-providers.md`), never just swap the setting under a live
collection.

## Develop and test

```bash
python -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt -r requirements-dev.txt
ML_EMBEDDING_BACKEND=hash ML_RERANK_BACKEND=embedding uvicorn app.main:app --port 8000
pytest -q && ruff check .
ML_TEST_REAL_MODELS=1 pytest tests/test_models.py   # downloads and checks the real models
```

Evaluate any OpenAI-compatible embedding endpoint on the RO/RU retrieval set:

```bash
python -m app.evaluate --url http://localhost:8000/v1 --model bge-m3 \
  --dataset ../../fixtures/eval/retrieval.json
```
