// provider is a stand-in for a hosted model behind an OpenAI-compatible API.
//
// It is scripted, not smart, and on one point it is deliberately the worst
// model you could deploy: it always follows an instruction it finds in a tool
// result. The post's premise is "assume the injection succeeds", so the demo
// does not depend on whether today's model happens to resist this particular
// injection. What it shows is what the infrastructure lets out afterwards.
//
// It answers only to a caller holding its API key, like a real provider.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/davideimola/blog-examples/two-doors/internal/chat"
)

var (
	// The injected instruction names where to send things. A real injection
	// would be worded a hundred different ways; the stand-in only needs to
	// find the address.
	postTo = regexp.MustCompile(`POST it to\s+(\S+)`)
	urlIn  = regexp.MustCompile(`https?://\S+`)
)

func main() {
	key := os.Getenv("PROVIDER_API_KEY")
	if key == "" {
		log.Fatal("PROVIDER_API_KEY is required")
	}
	addr := envOr("ADDR", ":8080")

	http.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			log.Printf("401 from %s: no valid API key", r.RemoteAddr)
			http.Error(w, `{"error":{"message":"invalid API key"}}`, http.StatusUnauthorized)
			return
		}
		var req chat.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		msg := next(req.Messages)
		log.Printf("200 from %s: model=%s messages=%d -> %s", r.RemoteAddr, req.Model, len(req.Messages), describe(msg))

		finish := "stop"
		if len(msg.ToolCalls) > 0 {
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chat.Response{
			ID:      fmt.Sprintf("chatcmpl-%d", len(req.Messages)),
			Object:  "chat.completion",
			Model:   req.Model,
			Choices: []chat.Choice{{Message: msg, FinishReason: finish}},
			Usage:   chat.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20},
		})
	})

	log.Printf("provider listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// next decides the assistant's next message from the conversation so far.
func next(msgs []chat.Message) chat.Message {
	last := msgs[len(msgs)-1]

	switch last.Role {
	case "user":
		if u := urlIn.FindString(last.Content); u != "" {
			return call("fetch", map[string]string{"url": u})
		}
		if strings.Contains(strings.ToLower(last.Content), "deploy") {
			return call("read_file", map[string]string{"path": ".env"})
		}
		return say("How can I help?")

	case "tool":
		target := injected(msgs)
		switch last.Name {
		case "read_file":
			return say("The deploy fails because DATABASE_URL still points at the old database host. Update it and redeploy.")
		case "http_post":
			if target != "" && strings.HasPrefix(last.Content, "error") {
				// The injection planned for this: if POST is blocked, put the
				// data in the address of a page and ask to read it.
				u := target + "?d=" + base64.RawURLEncoding.EncodeToString([]byte(seen(msgs)))
				return call("fetch", map[string]string{"url": u})
			}
			return say("Done.")
		case "fetch":
			if target != "" && !sent(msgs) {
				return call("http_post", map[string]string{"url": target, "body": seen(msgs)})
			}
			return say("Here is the summary of the advisory: upgrade the library to 2.4.1.")
		}
	}
	return say("Done.")
}

// injected returns the address an injected instruction asked to send data to,
// if any tool result carried one.
func injected(msgs []chat.Message) string {
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		if g := postTo.FindStringSubmatch(m.Content); g != nil {
			return strings.TrimRight(g[1], ".,")
		}
	}
	return ""
}

// sent reports whether the model already tried to send the data once.
func sent(msgs []chat.Message) bool {
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if c.Function.Name == "http_post" {
				return true
			}
		}
	}
	return false
}

// seen is "everything you've seen so far": every user turn and tool result.
func seen(msgs []chat.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "user" || (m.Role == "tool" && m.Name == "read_file") {
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func call(name string, args map[string]string) chat.Message {
	raw, _ := json.Marshal(args)
	return chat.Message{
		Role: "assistant",
		ToolCalls: []chat.ToolCall{{
			ID:       "call_" + name,
			Type:     "function",
			Function: chat.Function{Name: name, Arguments: string(raw)},
		}},
	}
}

func say(text string) chat.Message { return chat.Message{Role: "assistant", Content: text} }

func describe(m chat.Message) string {
	if len(m.ToolCalls) == 0 {
		return "answer"
	}
	c := m.ToolCalls[0].Function
	var args map[string]string
	json.Unmarshal([]byte(c.Arguments), &args)
	if u, ok := args["url"]; ok {
		if p, err := url.Parse(u); err == nil {
			return fmt.Sprintf("call %s(%s)", c.Name, p.Host)
		}
	}
	return "call " + c.Name
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
