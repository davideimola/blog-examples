# two-doors

An agent has two ways to send data out: the call to the model, and its tools. This demo runs the same tool-calling agent twice on a kind cluster, first with both doors open and then with both closed, against an injected instruction that tries to send its session to an attacker.

Nothing here needs a model or an API key. The "provider" is a scripted stand-in with an OpenAI-compatible API that always follows an instruction it finds in a tool result: the premise is that the injection succeeds, so the demo shows what the infrastructure lets out afterwards, not whether a model resisted.

| Piece | Where | What it plays |
|---|---|---|
| `agent` | `cmd/agent` | the agent: one session, tools `read_file`, `fetch`, `http_post` |
| `provider` | `cmd/provider` | a hosted model behind an API key |
| `internet` | `cmd/internet` | the advisory page with the injection, the ticket system, and the attacker's collector |
| `fetcher` | `cmd/fetcher` | the only pod allowed to read the open web; holds nothing |
| Envoy AI Gateway | `k8s/base/gateway.yaml` | the model door: model allowlist, provider key held by the gateway |
| squid | `k8s/base/tools.yaml` | the tool door for writes: forwards only to the ticket system |
| NetworkPolicy | `k8s/closed/egress.yaml` | default-deny egress for the agent, with the two doors as the only exceptions |

## Run it

Needs Docker, kind, kubectl and helm.

```sh
make up        # kind cluster, Envoy Gateway + AI Gateway, the world, the doors

make open      # the agent with its doors open
make logs      # the collector received the session, password included

make closed    # the same agent with both doors closed
make probe     # the ways around them: skip the proxy, skip the gateway, an undeclared model
make logs      # the collector received nothing

make down
```

With the doors closed, the injected `POST` gets a 403 from the proxy, a direct connection times out on the NetworkPolicy, and the provider can't be reached without the gateway (which is also the only holder of its key). The injection then tries its plan B: put the session in the query string of a URL and ask `fetch` to read it. The fetcher would happily GET it, so the agent's `fetch` tool accepts only URLs the user typed in the session (`FETCH_USER_URLS_ONLY`). Set it to `false` in `k8s/closed/agent.yaml` and run `make closed` again to watch the session leave through a GET.

Two things about kind that the manifests work around, both commented where they live: squid sizes its tables from kind's huge open-files limit and gets OOM-killed without `max_filedescriptors`, and kindnet lets a new pod's first packets through before it has learned the pod's address, so the agent waits a few seconds (`START_DELAY`) before it starts.

