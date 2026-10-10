// Command genkit shows what the Genkit Go Agents API writes to disk when a
// tool reads a secret.
//
// An agent backed by the file session store saves a snapshot at the end of
// every turn. Each snapshot is a JSON file holding the whole conversation so
// far, tool requests and tool responses included, so whatever a tool returned
// is copied into every snapshot written after it. Nothing prunes them unless
// the store is built with localstore.WithMaxPersistedChainLength, and even
// then the surviving snapshot still carries the secret, because each snapshot
// holds the full history.
//
// No API key is needed: a fake model asks for the read_file tool, then
// answers with text once it sees the tool response.
package main

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/firebase/genkit/go/ai"
	aix "github.com/firebase/genkit/go/ai/exp"
	"github.com/firebase/genkit/go/ai/exp/localstore"
	"github.com/firebase/genkit/go/genkit"
	genkitx "github.com/firebase/genkit/go/genkit/exp"
)

const (
	fixture    = "../fixtures/.env"
	secretName = "DB_PASSWORD"
)

type readFileInput struct {
	Path string `json:"path"`
}

func main() {
	ctx := context.Background()

	// The Agents API is gated behind the experimental flag.
	g := genkit.Init(ctx, genkit.WithExperimental())

	model := genkit.DefineModelAction(g, "demo/fake", &ai.ModelOptions{
		Label:    "Fake model",
		Supports: &ai.ModelSupports{Multiturn: true, Tools: true},
	}, fakeModel)

	readFile := genkit.DefineTool(g, "read_file", "Reads a file from the project.",
		func(ctx *ai.ToolContext, in readFileInput) (string, error) {
			b, err := os.ReadFile(fixture)
			if err != nil {
				return "", err
			}
			return string(b), nil
		})

	for _, run := range []struct {
		name string
		dir  string
		opts []localstore.FileStoreOption
	}{
		{"default store", "snapshots-default", nil},
		{"WithMaxPersistedChainLength(1)", "snapshots-pruned", []localstore.FileStoreOption{localstore.WithMaxPersistedChainLength(1)}},
	} {
		if err := demo(ctx, g, model, readFile, run.name, run.dir, run.opts...); err != nil {
			log.Fatalf("%s: %v", run.name, err)
		}
	}
}

func demo(ctx context.Context, g *genkit.Genkit, model ai.Model, readFile ai.Tool, name, dir string, opts ...localstore.FileStoreOption) error {
	// Start from an empty store so the listing only shows this run.
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clean store: %w", err)
	}
	store, err := localstore.NewFileSessionStore[any](dir, opts...)
	if err != nil {
		return fmt.Errorf("session store: %w", err)
	}

	agent := genkitx.DefineAgent(g, "helper-"+dir,
		aix.InlinePrompt{
			ai.WithModel(model),
			ai.WithSystem("You help with the project configuration."),
			ai.WithTools(readFile),
		},
		aix.WithSessionStore(store),
	)

	fmt.Printf("== %s (%s)\n", name, dir)
	var sessionID string
	for i, text := range []string{
		"What is in the .env file?",
		"Thanks. Anything else I should know?",
		"OK, that's all.",
	} {
		var inv []aix.InvocationOption[any]
		if sessionID != "" {
			inv = append(inv, aix.WithSessionID[any](sessionID))
		}
		out, err := agent.RunText(ctx, text, inv...)
		if err != nil {
			return fmt.Errorf("turn %d: %w", i+1, err)
		}
		if out.Error != nil {
			return fmt.Errorf("turn %d failed: %v", i+1, out.Error)
		}
		sessionID = out.SessionID
		fmt.Printf("turn %d > %s\n       < %s\n", i+1, text, out.Message.Text())
	}

	fmt.Printf("Files on disk for session %s:\n", sessionID)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Printf("  %-74s %5d bytes  %s x%d\n", path, len(b), secretName, strings.Count(string(b), secretName))
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk store: %w", err)
	}
	fmt.Println()
	return nil
}

// fakeModel asks for read_file when the last message is from the user and
// answers with text once the last message is the tool response.
//
// Like the real plugins (googlegenai, anthropic, compat_oai, ollama) it echoes
// the request on the response. The agent rebuilds the session history from
// ModelResponse.History(), which reads Request.Messages, so this is what puts
// the tool loop, and the tool output with it, into the snapshot.
func fakeModel(ctx context.Context, req *ai.ModelRequest, _ any, _ ai.ModelStreamCallback) (*ai.ModelResponse, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == ai.RoleTool {
		return &ai.ModelResponse{
			Message:      ai.NewModelTextMessage("I read the file. It holds the app's API key and database password."),
			FinishReason: ai.FinishReasonStop,
			Request:      req,
		}, nil
	}
	return &ai.ModelResponse{
		Message: ai.NewModelMessage(ai.NewToolRequestPart(&ai.ToolRequest{
			Name:  "read_file",
			Input: map[string]any{"path": ".env"},
		})),
		FinishReason: ai.FinishReasonStop,
		Request:      req,
	}, nil
}
