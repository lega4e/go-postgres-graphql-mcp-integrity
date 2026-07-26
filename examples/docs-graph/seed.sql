-- A small documentation corpus in PageIndex's shape: three documents, each a
-- tree of sections carrying a node id, a page range and a summary — plus the
-- citations *between* them, which is what makes this a graph.
--
-- The corpus is an engineering org's payments documentation: an architecture
-- doc, the incident review that followed an outage, and the runbook the review
-- forced changes to. The interesting edges are the ones that cross documents.
--
-- Ids are fixed and readable so a query result can be checked by eye.
-- ON CONFLICT DO NOTHING keeps re-seeding harmless.

BEGIN;

INSERT INTO documents (id, title, source, pages) VALUES
  ('d0000000-0000-0000-0000-000000000001', 'Payments Platform Architecture', 'docs/payments-architecture.pdf', 48),
  ('d0000000-0000-0000-0000-000000000002', 'Incident Review: Checkout Outage, 14 March', 'docs/incidents/2026-03-14.md', 12),
  ('d0000000-0000-0000-0000-000000000003', 'Payments On-call Runbook', 'wiki/payments/runbook', 21)
ON CONFLICT DO NOTHING;

-- Sections. nodeId is PageIndex's own identifier; the page range is its
-- start_index/end_index.
INSERT INTO sections (id, "nodeId", title, summary, "startPage", "endPage") VALUES
  -- Architecture (document 1)
  ('50000000-0000-0000-0000-000000000001', '0001', 'Payments Platform Architecture', 'Full architecture of the payments platform: ingress, the ledger, settlement and the retry machinery.', 1, 48),
  ('50000000-0000-0000-0000-000000000002', '0002', 'Request Ingress', 'How a checkout request enters the platform: the edge gateway, idempotency keys and rate limits.', 3, 11),
  ('50000000-0000-0000-0000-000000000003', '0003', 'Idempotency Keys', 'Every charge carries a client-supplied idempotency key; the gateway deduplicates on it for 24 hours.', 6, 9),
  ('50000000-0000-0000-0000-000000000004', '0004', 'The Ledger', 'Double-entry ledger: accounts, postings, and the invariant that every transaction sums to zero.', 12, 27),
  ('50000000-0000-0000-0000-000000000005', '0005', 'Ledger Write Path', 'A posting is written inside one transaction with its balance update; the write path is the platform''s only serialisable hot spot.', 16, 22),
  ('50000000-0000-0000-0000-000000000006', '0006', 'Settlement', 'Nightly settlement batches postings per processor and reconciles against processor statements.', 28, 39),
  ('50000000-0000-0000-0000-000000000007', '0007', 'Retries and Backoff', 'Failed processor calls retry with exponential backoff and a circuit breaker per processor.', 40, 48),

  -- Incident review (document 2)
  ('50000000-0000-0000-0000-000000000010', '0001', 'Incident Review: Checkout Outage, 14 March', 'Checkout was unavailable for 42 minutes after a ledger lock pile-up. Full timeline, cause and actions.', 1, 12),
  ('50000000-0000-0000-0000-000000000011', '0002', 'Timeline', 'Minute-by-minute account from first alert to recovery.', 2, 5),
  ('50000000-0000-0000-0000-000000000012', '0003', 'Root Cause', 'A retry storm against a degraded processor held ledger row locks open, serialising the write path until connections were exhausted.', 6, 9),
  ('50000000-0000-0000-0000-000000000013', '0004', 'Corrective Actions', 'Cap retries per processor, shed load at the gateway, and rewrite the runbook''s recovery steps.', 10, 12),

  -- Runbook (document 3)
  ('50000000-0000-0000-0000-000000000020', '0001', 'Payments On-call Runbook', 'What to do when payments alerts fire, by alert.', 1, 21),
  ('50000000-0000-0000-0000-000000000021', '0002', 'Alert: Ledger Lock Wait High', 'Diagnose and clear a ledger lock pile-up, including how to shed checkout load safely.', 4, 9),
  ('50000000-0000-0000-0000-000000000022', '0003', 'Alert: Processor Error Rate', 'Confirm a processor is degraded and trip its circuit breaker by hand if the automatic one has not.', 10, 15),
  ('50000000-0000-0000-0000-000000000023', '0004', 'Escalation', 'Who to wake, when, and what the payments incident commander needs.', 16, 21)
ON CONFLICT DO NOTHING;

-- Which document each section belongs to.
INSERT INTO has_section (source_id, target_id) VALUES
  ('d0000000-0000-0000-0000-000000000001', '50000000-0000-0000-0000-000000000001'),
  ('d0000000-0000-0000-0000-000000000001', '50000000-0000-0000-0000-000000000002'),
  ('d0000000-0000-0000-0000-000000000001', '50000000-0000-0000-0000-000000000003'),
  ('d0000000-0000-0000-0000-000000000001', '50000000-0000-0000-0000-000000000004'),
  ('d0000000-0000-0000-0000-000000000001', '50000000-0000-0000-0000-000000000005'),
  ('d0000000-0000-0000-0000-000000000001', '50000000-0000-0000-0000-000000000006'),
  ('d0000000-0000-0000-0000-000000000001', '50000000-0000-0000-0000-000000000007'),
  ('d0000000-0000-0000-0000-000000000002', '50000000-0000-0000-0000-000000000010'),
  ('d0000000-0000-0000-0000-000000000002', '50000000-0000-0000-0000-000000000011'),
  ('d0000000-0000-0000-0000-000000000002', '50000000-0000-0000-0000-000000000012'),
  ('d0000000-0000-0000-0000-000000000002', '50000000-0000-0000-0000-000000000013'),
  ('d0000000-0000-0000-0000-000000000003', '50000000-0000-0000-0000-000000000020'),
  ('d0000000-0000-0000-0000-000000000003', '50000000-0000-0000-0000-000000000021'),
  ('d0000000-0000-0000-0000-000000000003', '50000000-0000-0000-0000-000000000022'),
  ('d0000000-0000-0000-0000-000000000003', '50000000-0000-0000-0000-000000000023')
ON CONFLICT DO NOTHING;

-- The tree: PageIndex's nested `nodes`, flattened to parent → child edges.
INSERT INTO has_child (source_id, target_id) VALUES
  ('50000000-0000-0000-0000-000000000001', '50000000-0000-0000-0000-000000000002'),
  ('50000000-0000-0000-0000-000000000001', '50000000-0000-0000-0000-000000000004'),
  ('50000000-0000-0000-0000-000000000001', '50000000-0000-0000-0000-000000000006'),
  ('50000000-0000-0000-0000-000000000001', '50000000-0000-0000-0000-000000000007'),
  ('50000000-0000-0000-0000-000000000002', '50000000-0000-0000-0000-000000000003'),
  ('50000000-0000-0000-0000-000000000004', '50000000-0000-0000-0000-000000000005'),
  ('50000000-0000-0000-0000-000000000010', '50000000-0000-0000-0000-000000000011'),
  ('50000000-0000-0000-0000-000000000010', '50000000-0000-0000-0000-000000000012'),
  ('50000000-0000-0000-0000-000000000010', '50000000-0000-0000-0000-000000000013'),
  ('50000000-0000-0000-0000-000000000020', '50000000-0000-0000-0000-000000000021'),
  ('50000000-0000-0000-0000-000000000020', '50000000-0000-0000-0000-000000000022'),
  ('50000000-0000-0000-0000-000000000020', '50000000-0000-0000-0000-000000000023')
ON CONFLICT DO NOTHING;

-- The graph: citations, mostly across documents. This is the structure a
-- per-document tree cannot express.
INSERT INTO cites (source_id, target_id) VALUES
  -- The root cause leans on the ledger write path and on retry behaviour.
  ('50000000-0000-0000-0000-000000000012', '50000000-0000-0000-0000-000000000005'),
  ('50000000-0000-0000-0000-000000000012', '50000000-0000-0000-0000-000000000007'),
  -- ... and on the runbook step that was followed during the incident.
  ('50000000-0000-0000-0000-000000000012', '50000000-0000-0000-0000-000000000021'),
  -- Corrective actions point at what has to change.
  ('50000000-0000-0000-0000-000000000013', '50000000-0000-0000-0000-000000000007'),
  ('50000000-0000-0000-0000-000000000013', '50000000-0000-0000-0000-000000000021'),
  ('50000000-0000-0000-0000-000000000013', '50000000-0000-0000-0000-000000000002'),
  -- The runbook cites the architecture it is operating.
  ('50000000-0000-0000-0000-000000000021', '50000000-0000-0000-0000-000000000005'),
  ('50000000-0000-0000-0000-000000000022', '50000000-0000-0000-0000-000000000007'),
  ('50000000-0000-0000-0000-000000000023', '50000000-0000-0000-0000-000000000010'),
  -- And the architecture cites its own idempotency section from ingress.
  ('50000000-0000-0000-0000-000000000005', '50000000-0000-0000-0000-000000000003')
ON CONFLICT DO NOTHING;

COMMIT;
