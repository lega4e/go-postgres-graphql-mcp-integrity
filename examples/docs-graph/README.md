# docs-graph — a documentation index as a graph

Modelled on [PageIndex](https://github.com/VectifyAI/PageIndex), which turns a
document into a *semantic tree*: nodes carrying `title`, `node_id`,
`start_index`/`end_index` (a page range) and a `summary`, nested through a
`nodes` array. `Section` here is that node, and `has_child` is that nesting.

What a tree cannot hold is the interesting part: **sections cite sections,
across documents**. The corpus is three documents from one engineering org —
an architecture doc, the incident review that followed an outage, and the
runbook the review forced changes to — and the citations run between them.

```text
Document ──has_section──▶ Section ──has_child──▶ Section
                              │
                              └──cites──▶ Section   (usually in another document)
```

## Run it

```sh
docker compose up -d --build
docker compose --profile mcp build
claude --mcp-config .mcp.json
```

## Questions worth asking it

- *"What does the incident review's Root Cause section lean on?"* — one hop
  over `cites`, and the answer crosses into two other documents.
- *"Who cites the Ledger Write Path section?"* — `citedBy`, which is the same
  edge read backwards, and is how you find the blast radius of editing a
  section.
- *"Show me the runbook's sections and their page ranges"* — the tree, with
  PageIndex's `startPage`/`endPage` intact.

A worked exchange, run against this stack:

```graphql
{ sections(title: "Root Cause") { id title cites { title } } }
```
```json
{"sections": [{
  "id": "50000000-0000-0000-0000-000000000012",
  "title": "Root Cause",
  "cites": [
    {"title": "Ledger Write Path"},
    {"title": "Retries and Backoff"},
    {"title": "Alert: Ledger Lock Wait High"}
  ]}]}
```

Two of those three are in other documents, which is the whole point.
