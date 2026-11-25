# Access rules for data derived from Documents

Access is granted per Document, but Entities, Relations and Wiki Pages are built across Documents, so the boundary of what a Principal may read falls inside the scope that derived data is built over. We adopt these rules:

1. **Graph.** Postgres resolves the Principal to the set of Documents they can read, and that set is passed into every Cypher query. An Entity or Relation is returned only if at least one of its Mentions or evidence Chunks belongs to a readable Document.
2. **Wiki.** A Wiki Page is shown only to a Principal who can read every Document it was written from. Otherwise it is hidden, and the agent falls back to Chunks and the graph.
3. **Memory.** A Memory holds only what the Principal said themselves, never content taken from a Document, so losing access to a Document cannot leave its content behind in Memory.
4. **External Tools.** Once an agent turn has read a Restricted Document, External Tools are blocked for the rest of that turn. Every other External Tool call needs a Tool Approval.

Leakage rate on an adversarial question set is a column of the Ablation Matrix, so a change that breaks these rules shows up as a number.

## How this differs from Omnisc

Omnisc grants access per knowledge base (tenant roles owner/admin/contributor/viewer, plus knowledge bases shared to an organization). Its graph labels and its wiki are scoped to a knowledge base too, so anyone who can read the knowledge base can read every derived artifact in it. The access boundary and the scope that derived data is built over are the same thing, so Omnisc needs none of these rules. Trelis needs them because it moves the boundary down to the Document.

## Considered Options

- **Write the Wiki once per distinct combination of access.** Rejected: LLM cost grows with the number of combinations.
- **Show the merged Wiki Page to anyone who can read at least one source.** Rejected: that is Leakage by construction.

The rule we adopted makes the Wiki less useful to Principals with narrow access, and we accept that.
