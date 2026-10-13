# Fact-checking policy

The system verifies **claims**, not articles or outlets, and it is not a truth oracle. Models only
classify how a piece of evidence relates to a claim. Verdicts come from transparent, versioned
rules, and definitive labels in risky situations require a human.

## Pipeline

1. **Extraction.** Claims are extracted with exact quote spans (offsets are validated against the
   article text), attribution, type (statistic, decision, event, attribution, statement) and
   **checkability**: `checkable`, `opinion`, `prediction`, `satire`, `rhetorical`, `quote_only`.
   Only checkable claims are verified; satire and opinion pieces are not evidence.
2. **Evidence retrieval.** Independent documents are retrieved lexically, semantically and through
   the claim's events. Articles containing the claim and articles from the same publishers are
   excluded. When a new government, public-institution or fact-checker article arrives, related
   open claims are re-verified with that article included.
3. **Assessment.** The `evidence_assessment` route labels each document as `supports`,
   `contradicts` or `context`, with polarity checks (an official denial is not support).
4. **Independence.** Syndicated copies, duplicates and multiple articles from one publisher form
   one independence cluster; corroboration is counted per cluster. Primary (official) sources are
   flagged.
5. **Decision** (deterministic, `internal/factcheck/policy.go`, policy `factcheck`):

| Condition | Verdict |
|---|---|
| Not checkable | `not_checkable` |
| Independent support **and** contradiction | `disputed` |
| Enough independent contradiction (≥ `min_independent_contradicting`), no support | `refuted` |
| Enough independent support (≥ `min_independent_supporting`, or a primary source + ≥ `min_independent_supporting_with_primary`), no contradiction | `supported` |
| Anything else (including no evidence) | **`unverified`** |

   The default is `unverified`. Absence of evidence never yields `refuted`. Other states
   (`misleading_context`, `partially_supported`, `outdated`) are set by reviewers.

6. **Review gate.** Review is required when the verdict is in `review_required_verdicts`
   (default `refuted`, `misleading_context`, `disputed`), when the topic is high-risk (elections,
   war, health, terrorism, corruption, … from `high_risk_terms`) and the verdict is definitive, or
   when confidence is below `review_confidence_below`. Until review, readers see the claim as
   **`unverified` with review status `pending_review`**, never the proposed label.
7. **Publication.** `factcheck:review` (fact-checkers) can attach evidence, re-verify, set status
   and record decisions; `factcheck:publish` (editors) publishes. A fact-checker cannot publish a
   definitive label alone.
8. **Corrections.** Published verdicts are versioned. Corrections create a new version with a
   reason; history is public (`GET /v1/fact-checks/{id}` returns `history`).

## What readers see

`GET /v1/fact-checks/{id}` returns: the exact claim and where it appeared, verdict and review
status, a deterministic headline and explanation in the reader's language (built from evidence
signals only, with no generated allegations), limitations (for example "Only one independent
source supports the claim", "No primary/official document was found"), supporting and
contradicting evidence with publishers, links and dates, the policy version and models used, and
the correction history. There is never a bare "AI says fake".

`GET /v1/misinformation/trends` aggregates reviewed claims and published narratives.
Narratives (`/v1/admin/narratives`) group related claims and require publish permission.

## Interaction with other features

- Events hold contradicting updates for editorial approval before they reach timelines.
- Breaking alerts require corroborated, undisputed claims and no pending review
  ([event-processing.md](event-processing.md)).
- Chat retrieves fact-checks for its scope with their review status, so a pending item is
  presented as under review.

## Measured quality, not claimed accuracy

No accuracy figure for "fake news detection" is claimed. `newsctl eval` measures claim extraction
(checkable recall, rejection of opinions and predictions, span fidelity) and grounded answering on
small synthetic datasets for whichever models are routed, with gates that fail on regression.
Results are stored in `eval_runs` and shown at `/v1/admin/ai/evaluations`. Extending the datasets
with reviewed real cases is part of the handoff.

## Policy keys (`editorial_policies.factcheck`)

`policy_version`, `min_independent_supporting` (2), `min_independent_supporting_with_primary` (1),
`min_independent_contradicting` (1), `outdated_after_days` (365), `evidence_window_days` (30),
`review_required_verdicts`, `review_confidence_below` (0.6), `high_risk_terms`. Editing the policy
(`PUT /v1/admin/policies/factcheck`) is versioned and audited, and verdicts record the policy
version they were decided under.
