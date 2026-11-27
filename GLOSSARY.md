# Trelis

Trelis answers questions over a document collection and shows, with numbers, which retrieval techniques actually help.

## Language

### Documents and access

**Document**:
One ingested file, the unit that access is granted on.
_Avoid_: Knowledge, file (when meaning the ingested unit)

**Source File**:
The bytes a Principal uploaded for a Document, kept exactly as uploaded.
_Avoid_: Original, raw file, attachment

**Figure**:
An image inside a Document (a photo, chart or scanned table), taken out when the Document is parsed; readable only by a Principal who can read that Document.
_Avoid_: Media, image (in prose), picture, asset

**Chunk**:
A contiguous piece of a Document's text that is retrieved and cited on its own.
_Avoid_: Passage, segment

**Principal**:
The identity a question is asked as; it belongs to zero or more Groups.
_Avoid_: User (when meaning the asking identity), account, tenant

**Group**:
A named set of Principals that access can be granted to at once.
_Avoid_: Role, team

**Access List**:
The Principals and Groups allowed to read a Document; anything derived from that Document is readable only by them.
_Avoid_: Permission, ACL (in prose), RBAC

**Restricted Document**:
A Document whose Access List does not include everyone.
_Avoid_: Private document, confidential document

**Leakage**:
Any part of a Document that a Principal cannot read showing up in what Trelis returns to that Principal.
_Avoid_: Data leak, permission bug

### Knowledge graph and wiki

**Extraction**:
The entities and relations an LLM read out of one Chunk, kept as the raw record everything graph-shaped is rebuilt from.
_Avoid_: Graph data, triples

**Entity**:
One real-world thing or concept, the same across all Documents that mention it (e.g. "Bộ Tài chính" is one Entity, however many Documents name it).
_Avoid_: Node, canonical entity (in prose), concept (as a separate kind)

**Mention**:
One appearance of an Entity in one Chunk, which is the evidence that Entity is known through.
_Avoid_: Occurrence, reference

**Relation**:
A stated link between two Entities, backed by the Chunks it was read from.
_Avoid_: Edge, relationship, triple

**Entity Resolution**:
Deciding which Mentions, across Documents, are the same Entity.
_Avoid_: Dedup, merging, linking

**Wiki Page**:
A read-only page Trelis writes from its sources, cited back to Chunks; readable only by a Principal who can read every Document it was written from.
_Avoid_: Article, note

**Summary Page**:
The Wiki Page for one Document.
_Avoid_: Document summary

**Entity Page**:
The Wiki Page for one Entity, written from all of that Entity's Mentions.
_Avoid_: Concept page, topic page

**Index Page**:
The Wiki Page that introduces the whole Corpus and points into the other Wiki Pages.
_Avoid_: Home page, table of contents

### Conversations

**Conversation History**:
The turns of the current conversation, carried forward (and compacted) so follow-up questions make sense.
_Avoid_: Short-term memory, context

**Memory**:
A fact or preference about one Principal, kept across conversations and recalled when relevant; never visible to another Principal.
_Avoid_: Long-term memory, profile

### Answering

**Pipeline Answer**:
An answer produced by a fixed sequence of retrieval and generation steps.
_Avoid_: RAG mode, simple mode

**Agent Answer**:
An answer produced by a model that decides, step by step, which tools to call before answering.
_Avoid_: Agentic mode, deep mode

**External Tool**:
A tool offered by an MCP server outside Trelis that an agent is allowed to call.
_Avoid_: Plugin, integration, MCP tool (when meaning only outside ones)

**Tool Approval**:
A Principal's yes or no to one External Tool call, given before the call runs.
_Avoid_: Confirmation, consent

### Corpora and evaluation

**Corpus**:
A set of documents ingested together so they can be searched and questioned as one body of knowledge.
_Avoid_: Dataset (when meaning the documents), collection

**Benchmark Corpus**:
A public Corpus with published questions and labeled evidence (MultiHop-RAG), used to produce numbers that others can compare against.
_Avoid_: Test data

**Demo Corpus**:
The Vietnamese legal documents (laws, decrees, circulars and the references between them) used to show Trelis on a real domain.
_Avoid_: Sample data

**Golden Set**:
A fixed list of questions, each with its expected answer and the evidence that supports it, against which Trelis is scored.
_Avoid_: Test set, QA pairs

**Retrieval Technique**:
One way of finding evidence (e.g. BM25, dense, hybrid fusion, rerank, query rewrite, graph search, agentic search) that can be switched on or off on its own.
_Avoid_: Strategy, mode, feature

**Retrieval Config**:
A named combination of Retrieval Techniques that is switched on together.
_Avoid_: Pipeline, preset

**Reciprocal Rank Fusion (RRF)**:
The way several Retrieval Techniques' ranked lists become one: a Chunk scores the sum of 1/(k + rank) over the lists that carry it, with 1-based ranks and k a per-request constant (default 60).
_Avoid_: Score blending, rank merge

**Ablation Run**:
One scoring of a Golden Set under one Retrieval Config, recording quality, latency and cost.
_Avoid_: Eval job, experiment

**Ablation Matrix**:
The table of Ablation Runs across Retrieval Configs that shows what each Retrieval Technique adds.
_Avoid_: Leaderboard, benchmark results

**Grader**:
One check that scores an answer on one aspect, either by rule or by a judge model, and says when it could not score.
_Avoid_: Metric (when meaning the check itself), evaluator, scorer

**Slice**:
A named subset of a Golden Set (e.g. comparison questions, three-hop questions, adversarial access questions) reported on its own.
_Avoid_: Segment, bucket, category

**Failure Attribution**:
Labeling each wrong answer by the stage that failed: retrieval miss (the evidence was never retrieved), generation error (evidence retrieved, answer wrong) or citation error (answer right, cited wrongly).
_Avoid_: Error analysis, root cause

**Cost per Correct Answer**:
What a Retrieval Config spends per question, divided by the share of questions it answers correctly.
_Avoid_: Cost efficiency, ROI
