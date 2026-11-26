package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"tokenanalyse/internal/collectors"
	"tokenanalyse/internal/config"
	"tokenanalyse/internal/server"
	"tokenanalyse/internal/storage"
	syncpkg "tokenanalyse/internal/sync"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx       context.Context
	db        *storage.DB
	identity  config.Identity
	backend   *http.Server
	listener  net.Listener
	backendMu sync.Mutex
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	dbPath := config.DefaultDBPath()
	db, err := storage.Open(dbPath)
	if err != nil {
		runtime.LogError(ctx, "open database: "+err.Error())
		return
	}
	a.db = db
	identity, err := config.LoadIdentity(dbPath, "")
	if err != nil {
		runtime.LogError(ctx, "load identity: "+err.Error())
		return
	}
	a.identity = identity
	_ = db.UpsertDevice(identity.Register())
	a.startBackend()
	go a.backgroundCollector()
}

func (a *App) shutdown(ctx context.Context) {
	a.backendMu.Lock()
	defer a.backendMu.Unlock()
	if a.backend != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.backend.Shutdown(shutdownCtx)
	}
	if a.db != nil {
		_ = a.db.Close()
	}
}

func (a *App) backendHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.backendMu.Lock()
		var address string
		if a.listener != nil {
			address = a.listener.Addr().String()
		}
		a.backendMu.Unlock()
		if address == "" {
			http.Error(w, "backend not ready", http.StatusServiceUnavailable)
			return
		}
		target, _ := url.Parse("http://" + address)
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	})
}

func (a *App) startBackend() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		runtime.LogError(a.ctx, err.Error())
		return
	}
	state := &server.AppState{DB: a.db, Version: config.Version}
	a.listener = listener
	a.backend = &http.Server{Handler: server.NewRouter(state), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = a.backend.Serve(listener) }()
}

func (a *App) backgroundCollector() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		a.CollectNow()
		<-ticker.C
	}
}

func (a *App) CollectNow() map[string]any {
	if a.db == nil {
		return map[string]any{"error": "database not ready"}
	}
	ctx := collectors.DetectContext(a.identity.DeviceID)
	cursors, _ := a.db.LoadCursors()
	result := collectors.BuiltinRegistry().CollectAll(ctx, cursors)
	inserted, err := a.db.InsertEvents(result.Events, true)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	_ = a.db.SaveCursors(result.Cursors)
	return map[string]any{"files": result.FilesScanned, "events": len(result.Events),
		"inserted": inserted.Inserted, "skipped": inserted.Skipped, "errors": result.Errors}
}

func (a *App) SyncNow(serverURL, token string) map[string]any {
	if a.db == nil {
		return map[string]any{"error": "database not ready"}
	}
	cfg := syncpkg.DefaultConfig()
	if serverURL != "" {
		cfg.Mode = syncpkg.ModeServer
		cfg.Server = syncpkg.Server{BaseURL: serverURL, Token: token}
	} else {
		return map[string]any{"error": "server URL is empty"}
	}
	report, err := syncpkg.NewRunner(a.db, cfg, a.identity.DeviceID).RunOnce(context.Background())
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"uploaded": report.Uploaded, "downloaded": report.Downloaded,
		"duplicates": report.Duplicates, "conflicts": report.Conflicts, "errors": report.Errors}
}

func (a *App) Status() map[string]any {
	return map[string]any{"deviceId": a.identity.DeviceID, "deviceName": a.identity.Name,
		"version": config.Version, "backend": a.listener.Addr().String()}
}

func (a *App) LogInfo(message string) { runtime.LogInfo(a.ctx, message) }
