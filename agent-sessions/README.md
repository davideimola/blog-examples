# agent-sessions

What AI agent frameworks keep on disk after a conversation, and what is left after you delete it.

`fixtures/.env` holds fake secrets that the demos feed to the agent as tool output.

## adk

ADK Go (`google.golang.org/adk/v2` v2.5.0) with the database session service on SQLite. Deleting a session leaves its events, tool output included, in the `events` table, because the cascade relies on SQLite foreign keys that are off by default.

```sh
cd adk
go run .
sqlite3 adk-default.db 'select content from events;'
```

No model or API key needed.

## genkit

Genkit Go (`github.com/firebase/genkit/go` v1.13.1) Agents API with the file session store. Every turn writes a new JSON snapshot holding the full conversation, tool output included, so a secret a tool read once is copied into every later snapshot. Old snapshots are never pruned by default. `WithMaxPersistedChainLength(1)` keeps only the latest one, which still holds the secret.

```sh
cd genkit
go run .
grep -o 'DB_PASSWORD=[^\\]*' snapshots-*/global/*.json
```

No model or API key needed: a fake model drives the tool loop.
