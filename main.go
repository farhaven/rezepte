//go:generate sqlc generate
package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"time"

	"git.sr.ht/~farhaven/rezepte/query"
	_ "modernc.org/sqlite"
)

type UI struct {
	mux *http.ServeMux
	db  *sql.DB
	log *slog.Logger
}

func newUI(db *sql.DB, log *slog.Logger) UI {
	u := UI{
		mux: http.NewServeMux(),
		db:  db,
		log: log,
	}

	u.mux.HandleFunc("/", u.index)

	return u
}

func (u UI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mux.ServeHTTP(w, r)
}

func (u UI) index(w http.ResponseWriter, r *http.Request) {

}

//go:embed query/schema*.sql
var dbSchema embed.FS

func openDB(ctx context.Context, path string, log *slog.Logger) (*sql.DB, error) {
	pragmas := []string{}

	path += "?"
	for _, p := range pragmas {
		path += "_pragma=" + p + "&"
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening db: %w", err)
	}

	migrations, err := fs.Glob(dbSchema, "query/schema*.sql")
	if err != nil {
		return nil, fmt.Errorf("listing migrations: %w", err)
	}

	sort.Strings(migrations)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("opening tx: %w", err)
	}

	defer func() {
		err := tx.Rollback()
		if err != nil {
			log.Error("rollback failed", "error", err)
		}
	}()

	// Create table for migrations. Can't do that in a migration because that's a circle.
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS migrations (name text unique not null)`)
	if err != nil {
		return nil, fmt.Errorf("creating migrations table: %w", err)
	}

	q := query.New(tx)

	for _, m := range migrations {
		alreadyApplied, err := q.MigrationApplied(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("checking for migration state: %w", err)
		}

		if alreadyApplied != 0 {
			log.Info("migration already applied", "name", m)
			continue
		}

		content, err := fs.ReadFile(dbSchema, m)
		if err != nil {
			return nil, fmt.Errorf("reading migration %q: %w", m, err)
		}

		log.Info("applying migration", "name", m)

		_, err = tx.ExecContext(ctx, string(content))
		if err != nil {
			return nil, fmt.Errorf("applying migration %q: %w", m, err)
		}

		err = q.MarkMigrationApplied(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("marking migration as applied: %w", err)
		}
	}

	err = tx.Commit()
	if err != nil {
		return nil, fmt.Errorf("comitting migration transaction: %w", err)
	}

	return nil, errors.New("not yet")
}

func main() {
	dbPath := flag.String("dbpath", "state.db", "Path to database.")
	debug := flag.Bool("debug", false, "Enable debug logging.")
	listenAddr := flag.String("listen", ":8080", "Listening address.")

	flag.Parse()

	var logLevel slog.LevelVar
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: &logLevel,
	}))

	if *debug {
		logLevel.Set(slog.LevelDebug)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	db, err := openDB(ctx, *dbPath, log)
	if err != nil {
		log.Error("failed to open database", "error", err)
		os.Exit(1)
	}

	s := http.Server{
		Addr:    *listenAddr,
		Handler: newUI(db, log),
	}

	go func() {
		<-ctx.Done()

		log.Info("got interrupt, shutting down.")

		sCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		err := s.Shutdown(sCtx)
		if err != nil {
			log.Error("failed to shut down HTTP server", "error", err)
		}
	}()

	log.Info("running HTTP server", "addr", *listenAddr)

	err = s.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("failed to run HTTP server", "error", err)
	}
}
