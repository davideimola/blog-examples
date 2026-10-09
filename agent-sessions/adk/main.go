// Command adk shows that deleting an ADK Go session stored in SQLite leaves
// its events behind unless foreign keys are enabled on the connection.
//
// The database session service relies on an ON DELETE CASCADE foreign key to
// remove a session's events. SQLite only enforces foreign keys when the
// connection turns them on (PRAGMA foreign_keys), and ADK does not do it, so
// Delete removes the session row and silently keeps the events, including
// whatever the tools returned.
//
// No model or API key is needed: the program talks to the session service
// directly, the way the runner does after each turn.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/genai"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	appName = "agent-sessions-demo"
	userID  = "user-42"
)

func main() {
	ctx := context.Background()

	secret, err := os.ReadFile("../fixtures/.env")
	if err != nil {
		log.Fatalf("read fixture: %v", err)
	}

	for _, run := range []struct {
		name string
		dsn  string
	}{
		{"default DSN", "file:adk-default.db"},
		{"foreign keys on", "file:adk-fk.db?_pragma=foreign_keys(1)"},
	} {
		if err := demo(ctx, run.name, run.dsn, string(secret)); err != nil {
			log.Fatalf("%s: %v", run.name, err)
		}
	}
}

func demo(ctx context.Context, name, dsn, secret string) error {
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	svc, err := database.NewSessionServiceFromDB(db)
	if err != nil {
		return fmt.Errorf("session service: %w", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	created, err := svc.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	sess := created.Session

	// What the runner appends when a tool returns: a function response
	// carrying the tool output, here the content of a .env file.
	ev := session.NewEvent(ctx, "invocation-1")
	ev.Author = "read_file"
	ev.LLMResponse.Content = genai.NewContentFromParts([]*genai.Part{
		genai.NewPartFromFunctionResponse("read_file", map[string]any{"content": secret}),
	}, genai.RoleUser)
	if err := svc.AppendEvent(ctx, sess, ev); err != nil {
		return fmt.Errorf("append event: %w", err)
	}

	if err := svc.Delete(ctx, &session.DeleteRequest{AppName: appName, UserID: userID, SessionID: sess.ID()}); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}

	var sessions, events int64
	db.Table("sessions").Where("id = ?", sess.ID()).Count(&sessions)
	db.Table("events").Where("session_id = ?", sess.ID()).Count(&events)

	fmt.Printf("== %s (%s)\n", name, dsn)
	fmt.Printf("Delete returned no error. Rows left for session %s: sessions=%d events=%d\n", sess.ID(), sessions, events)
	if events > 0 {
		var content string
		db.Table("events").Select("content").Where("session_id = ?", sess.ID()).Limit(1).Scan(&content)
		fmt.Printf("Orphaned event content: %s\n", content)
	}
	fmt.Println()
	return nil
}
