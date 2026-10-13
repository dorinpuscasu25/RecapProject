# Sources and categories

Both are data managed through audited admin APIs. Nothing about which outlets are crawled or how
news is categorized is hardcoded.

## Publishers, permissions and endpoints

**Publisher** (`/v1/admin/publishers`): name, canonical domain, country, languages, type
(`newsroom`, `government`, `public_institution`, `fact_checker`, `other`), status. The type matters
downstream: government and public-institution documents count as primary sources in
fact-checking (weighed, not treated as infallible) and trigger re-verification of related open
claims.

**Permission** (`PUT /v1/admin/publishers/{id}/permissions`, optionally per endpoint): legal basis,
`permission_status` (`pending`, `granted`, `restricted`, `denied`, `revoked`), what is allowed
(crawl, store full text, store raw HTML, summarize, embed, snippets with max length, audio),
robots and terms review status, retention, attribution text. Enforcement:

- An endpoint can only be **enabled** when its publisher is active and a granted permission allows
  crawling with robots/terms not `blocked`.
- Full text, raw snapshots, embeddings, summaries and audio are produced only if allowed;
  otherwise the system keeps metadata and links.
- Attribution text accompanies source links.

**Endpoint** (`/v1/admin/sources`): type `rss`, `atom`, `json_feed`, `sitemap`, `html_list` or
`fixture`; entry URL; polling interval (60 s – 7 days); priority; languages; category hints and an
allow-list of categories; allowed hosts (beyond the publisher domain); URL blocklist; whether to
fetch article pages; extraction profile; rate limit, concurrency, timeout, max items per poll;
retry policy; crawl window. Lifecycle: created `paused`; `enabled` ⇄ `paused`;
`pending_permission` while rights are being clarified; `disabled`; `archived`.

Not supported, and therefore rejected at validation rather than accepted and silently ignored:
`official_api` endpoints (they need a source-specific adapter), headless rendering, and
`js_required` sources.

### Extraction profiles

`/v1/admin/extraction-profiles`: CSS selectors for title, content, date, canonical URL, author,
cleanup and, for `html_list` endpoints, **link selectors** that find article links on a section
page. Selectors are syntax-checked on save; profiles carry HTML fixtures with expected values and
`POST /{id}/validate` runs them. Without a profile, extraction uses readability-style heuristics
plus OpenGraph and JSON-LD metadata, reporting a confidence.

### Testing a source before enabling it

- `POST /v1/admin/sources/test` (unsaved config) or `/v1/admin/sources/{id}/test` (saved): fetch,
  detect format, list items with extracted previews, permission warnings and errors. Nothing is
  persisted.
- `POST /v1/admin/sources/preview-url`: extract one page.

### Crawling, politeness and safety

- The scheduler enqueues due endpoints (`next_poll_at`); `POST /{id}/crawl` runs one now.
- Live HTTP crawling is off unless `CRAWL_LIVE_ENABLED=true` (and the `live_crawling` flag);
  fixture endpoints always work.
- robots.txt is fetched and cached per host; disallowed paths are skipped. Conditional requests
  (ETag/Last-Modified), per-endpoint rate limits and response size caps apply.
- **SSRF protection**: URLs are validated on save and again at dial time (resolved IPs checked
  against private, loopback, link-local, metadata and other reserved ranges; redirects re-checked;
  only http/https; no credentials in URLs). `CRAWL_ALLOW_PRIVATE_NETWORKS` exists for tests and is
  refused in production.
- Item URLs must be on the publisher's domain or the endpoint's `allowed_hosts`.

### Health

`GET /v1/admin/sources/{id}/health` and `/runs`: last success and error, consecutive failures,
runs and failed runs in 24 h, parse coverage (items that became articles), duplicate rate,
freshness lag, articles total and last 24 h, anomalies (e.g. repeated failures). A run whose items
fail is `partial`, and the endpoint is reported `degraded` when half or more of its items fail or
all are rejected, so a source that parses but yields nothing does not look healthy.
`GET /v1/admin/health/source-metrics` summarizes all sources.

### Articles

Each item becomes an article with canonical URL, revision history (`article_versions`),
language detection, dedupe (exact canonical URL, content hash, SimHash near-duplicates and
syndication; duplicates keep provenance), corrections and retractions detected from content.
Moderation: `/v1/admin/articles` (filter by status), quarantine, release, retry, withdraw (legal
takedown; affected events refreshed, search documents deleted, fact-check evidence superseded),
editorial category override, reprocess.

### Synthetic fixtures

`fixtures/sources.json` lists the synthetic local sources loaded by `newsctl ingest-fixtures`.
Fixture endpoints read `fixtures/feeds/*.xml` and `fixtures/pages/<host>/…`. All content is
invented. `FIXTURE_TIME_ANCHOR` (development and test only) shifts fixture dates relative to now
so that recency windows behave the same on any day.

## Categories (taxonomy)

`/v1/admin/categories` (permission `categories:write`):

- **Fields**: slug, parent, names and descriptions per language (ro, ru, en), editorial
  description, icon, display order, status (`active`, `hidden`, `archived`, `merged`),
  classification hints, aliases, positive and negative examples, boost, `auto_classify`, version.
- **Tree operations**: reparent (cycle-checked), reorder siblings, activate, hide, archive.
- **Merge** into another category: assignments and user interests move; a redirect is kept so
  old ids resolve.
- **Versions**: every change bumps the taxonomy version recorded on each AI assignment.
- **Reclassify** (`POST /v1/admin/categories/reclassify`): a job over a scope (time range,
  categories, publisher) with `dry_run` reporting proposed changes. Editorial assignments are never
  overwritten by AI.

Users see the active tree (`GET /v1/categories/tree?lang=ru`) and follow categories during
onboarding; they cannot create categories.

Classification is a task like any other: the classifier receives the current active categories
(with hints, aliases and examples) on every call, and its output is rejected if it names a category
that is not in the taxonomy. Adding "Energy → Electricity prices" through the API is enough for the
next article to be classified into it (acceptance scenario 2).

The seed provides a Moldova-focused starter taxonomy (politics, economy, justice, education,
health, technology, agriculture, energy → electricity prices, security, culture, sport, Chișinău /
local, EU integration) that can be replaced entirely through the API.
