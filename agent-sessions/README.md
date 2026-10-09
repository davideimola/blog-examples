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

Genkit Go (`github.com/firebase/genkit/go` v1.13.1) Agents API with the file session store. Work in progress.
