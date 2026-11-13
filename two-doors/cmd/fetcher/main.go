// fetcher is the agent's only way to read the open web once the agent's own
// egress is closed. It runs in a namespace of its own, with no session, no
// secrets and no credentials, and it can do one thing: GET a URL and return
// the text. Whatever an injected page says, the pod that reads it holds
// nothing worth stealing.
//
// It cannot tell a URL that carries data from one that doesn't, so a GET with
// a query string is still a way out. The post says so, and the demo shows it.
package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

const maxBody = 64 << 10

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	client := &http.Client{Timeout: 10 * time.Second}

	http.HandleFunc("GET /fetch", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("url")
		if target == "" {
			http.Error(w, "url is required", http.StatusBadRequest)
			return
		}
		log.Printf("GET %s", target)
		resp, err := client.Get(target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.Copy(w, io.LimitReader(resp.Body, maxBody))
	})

	log.Printf("fetcher listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
