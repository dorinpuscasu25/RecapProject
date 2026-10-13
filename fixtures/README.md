# Fixtures — SYNTHETIC CONTENT

Everything under `fixtures/` is **synthetic**, written for deterministic tests and local demos.
Publishers (`*-exemplu.md`, `*-primer.md`, `rapid-portal.md`), quotes, figures and decisions are
invented and do **not** describe real events. Public institutions are referenced only so the
entity gazetteer and evidence rules can be exercised.

| File | Publisher (synthetic) | Language | Purpose |
|---|---|---|---|
| `feeds/stiri-exemplu.xml` | Știri Exemplu (newsroom) | ro | Electricity tariff decision (event E1) + separate gas tariff decision (E2) |
| `feeds/novosti-primer.xml` | Новости Пример (newsroom) | ru | Same E1 decision in Russian (cross-language grouping) |
| `feeds/anre-exemplu.xml` + `pages/anre-exemplu.md/…` | ANRE exemplu (government) | ro | Official primary source for E1 (HTML extraction path) |
| `feeds/ziar-exemplu-day2.xml` | Ziar Exemplu (newsroom) | ro | Day-2 article repeating known facts (no material update) |
| `feeds/anre-exemplu-day3.xml` + page | ANRE exemplu | ro | Revised figures (material change / contradiction) |
| `feeds/rapid-portal.xml` | Portal Rapid (newsroom) | ro | False pension claim, unverifiable airport claim, satire |
| `feeds/mf-exemplu.xml` + page | Ministerul Finanțelor exemplu (government) | ro | Official denial of the pension claim |
| `eval/*.json` | — | ro/ru | Offline evaluation corpus with gold labels |
