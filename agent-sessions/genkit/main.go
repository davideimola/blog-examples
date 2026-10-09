// Command genkit shows what the Genkit Go Agents API writes to disk when a
// tool reads a secret.
//
// TODO(davide):
//  1. genkit.Init with genkit.WithExperimental() (the Agents API is gated).
//  2. A fake model (genkit.DefineModel) that asks for the read_file tool on the
//     first call and answers with text once it sees the tool response. No API key.
//  3. A read_file tool that returns ../fixtures/.env.
//  4. An agent with aix.WithSessionStore(localstore.NewFileSessionStore(...)).
//  5. Three turns on the same session, then list the snapshot files and grep
//     them for DB_PASSWORD.
//
// References in the Genkit repo (tag go/v1.13.1):
//   - go/samples/basic-agents/pirate.go: DefineAgent with an inline prompt
//   - go/samples/basic-agents/main.go: file session store setup (mustStore)
//   - go/ai/exp/localstore/option.go: WithMaxPersistedChainLength, WithSnapshotPathPrefix
package main

func main() {}
