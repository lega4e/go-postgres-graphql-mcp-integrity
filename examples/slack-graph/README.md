# slack-graph — team chat as a graph

Chat is a graph pretending to be a list. Channels have members, threads live in
channels, messages live in threads, people tag people, and threads point at
earlier threads:

```text
Person ──member_of──▶ Channel ◀──in_channel── Thread ──refers_to──▶ Thread
   │                                             ▲
   └──authored──▶ Message ──in_thread───────────┘
                     │
                     └──mentions──▶ Person
```

The corpus is a week around the same payments incident the
[`docs-graph`](../docs-graph) example documents: three channels, seven people,
five threads that refer to each other, and the mentions that pulled people in.

## Run it

```sh
docker compose up -d --build          # postgres, migrate, seed, and the server
claude --mcp-config .mcp.json         # the server is at http://localhost:8767/mcp
```

## Questions worth asking it

- *"Which earlier threads does the postmortem refer to?"* — `refersTo`, the
  edge that makes chat history navigable.
- *"Which messages mention grace, and who wrote them?"* — `mentionedIn` plus
  `author`, two hops, one round trip.
- *"Who is in #incidents?"* — `members`, read backwards through `member_of`.

The mention edges are the point: **none of the messages that mention Grace
contain the string "grace"**. The edge carries information the message body
does not, so a text search over the same corpus cannot answer the question —
which is the argument for storing chat as a graph in the first place.
