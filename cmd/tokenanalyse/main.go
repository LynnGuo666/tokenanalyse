package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tokenanalyse/internal/collectors"
	"tokenanalyse/internal/config"
	"tokenanalyse/internal/model"
	"tokenanalyse/internal/pricing"
	"tokenanalyse/internal/server"
	"tokenanalyse/internal/storage"
	syncpkg "tokenanalyse/internal/sync"
)

var version = "dev"

func main() {
	config.Version = version
	if len(os.Args) < 2 {
		usage()
		return
	}
	switch os.Args[1] {
	case "serve":
		serveCmd(os.Args[2:])
	case "collect":
		collectCmd(os.Args[2:])
	case "sync":
		syncCmd(os.Args[2:])
	case "node":
		nodeCmd(os.Args[2:])
	case "summary":
		summaryCmd(os.Args[2:])
	case "devices":
		devicesCmd(os.Args[2:])
	case "pricing":
		pricingCmd(os.Args[2:])
	case "import-cc-switch":
		importCCCmd(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `TokenAnalyse - AI CLI token usage analytics

Usage:
  tokenanalyse serve [flags]
  tokenanalyse collect [flags]
  tokenanalyse sync [flags]
  tokenanalyse node [flags]
  tokenanalyse summary [flags]
  tokenanalyse devices [flags]
  tokenanalyse pricing <show|sync-models-dev|import|reprice|aliases> [flags]
  tokenanalyse import-cc-switch [flags]
  tokenanalyse version`)
}

func openDB(path string) (*storage.DB, error) {
	if path == "" {
		path = config.DefaultDBPath()
	}
	if err := config.ValidateDBPath(path); err != nil {
		return nil, err
	}
	db, err := storage.Open(path)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func identityFor(dbPath, name string) (config.Identity, error) {
	if dbPath == "" {
		dbPath = config.DefaultDBPath()
	}
	return config.LoadIdentity(dbPath, name)
}

func registerIdentity(db *storage.DB, identity config.Identity) error {
	return db.UpsertDevice(identity.Register())
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbPath := fs.String("db", config.DefaultDBPath(), "SQLite database path")
	listen := fs.String("listen", "127.0.0.1:8787", "listen address")
	token := fs.String("token", os.Getenv("TM_API_TOKEN"), "API token")
	fs.Parse(args)
	db, err := openDB(*dbPath)
	must(err)
	defer db.Close()
	state := &server.AppState{DB: db, Token: *token, Version: version}
	httpServer := &http.Server{Addr: *listen, Handler: server.NewRouter(state), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("TokenAnalyse server listening on %s", *listen)
	must(httpServer.ListenAndServe())
}

func collectCmd(args []string) {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	dbPath := fs.String("db", config.DefaultDBPath(), "SQLite database path")
	deviceName := fs.String("device-name", "", "device display name")
	interval := fs.Duration("interval", 0, "collection interval, e.g. 5m; omit for one shot")
	fs.Parse(args)
	db, err := openDB(*dbPath)
	must(err)
	defer db.Close()
	identity, err := identityFor(*dbPath, *deviceName)
	must(err)
	must(registerIdentity(db, identity))
	for {
		result := collectOnce(db, identity)
		log.Printf("collection: files=%d events=%d errors=%d", result.FilesScanned, len(result.Events), len(result.Errors))
		for _, message := range result.Errors {
			log.Printf("collector: %s", message)
		}
		if *interval <= 0 {
			return
		}
		time.Sleep(*interval)
	}
}

func collectOnce(db *storage.DB, identity config.Identity) collectors.Result {
	ctx := collectors.DetectContext(identity.DeviceID)
	cursors, _ := db.LoadCursors()
	result := collectors.BuiltinRegistry().CollectAll(ctx, cursors)
	if len(result.Events) > 0 {
		inserted, err := db.InsertEvents(result.Events, true)
		if err != nil {
			result.Errors = append(result.Errors, err.Error())
		} else {
			log.Printf("events inserted=%d skipped=%d", inserted.Inserted, inserted.Skipped)
		}
	}
	if len(result.Cursors) > 0 {
		if err := db.SaveCursors(result.Cursors); err != nil {
			result.Errors = append(result.Errors, err.Error())
		}
	}
	return result
}

func syncCmd(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	dbPath := fs.String("db", config.DefaultDBPath(), "SQLite database path")
	serverURL := fs.String("server", os.Getenv("TM_SYNC_SERVER"), "upstream server URL")
	token := fs.String("token", os.Getenv("TM_DEVICE_TOKEN"), "device token")
	webdav := fs.String("webdav", os.Getenv("TM_WEBDAV_URL"), "WebDAV URL")
	webdavUser := fs.String("webdav-user", os.Getenv("TM_WEBDAV_USER"), "WebDAV username")
	webdavPassword := fs.String("webdav-password", os.Getenv("TM_WEBDAV_PASSWORD"), "WebDAV password")
	remoteRoot := fs.String("remote-root", "tokenanalyse", "WebDAV remote root")
	profile := fs.String("profile", "default", "sync profile")
	interval := fs.Duration("interval", 0, "sync interval, e.g. 5m; omit for one shot")
	fs.Parse(args)
	db, err := openDB(*dbPath)
	must(err)
	defer db.Close()
	identity, err := identityFor(*dbPath, "")
	must(err)
	cfg := syncpkg.DefaultConfig()
	if *serverURL != "" {
		cfg.Mode = syncpkg.ModeServer
		cfg.Server = syncpkg.Server{BaseURL: *serverURL, Token: *token}
	} else if *webdav != "" {
		cfg.Mode = syncpkg.ModeWebDAV
		cfg.WebDAV = syncpkg.WebDAV{BaseURL: *webdav, Username: *webdavUser, Password: *webdavPassword,
			RemoteRoot: *remoteRoot, Profile: *profile}
	} else {
		must(fmt.Errorf("provide --server or --webdav"))
	}
	runner := syncpkg.NewRunner(db, cfg, identity.DeviceID)
	for {
		report, err := runner.RunOnce(context.Background())
		if err != nil {
			log.Printf("sync error: %v", err)
		} else {
			log.Printf("sync uploaded=%d downloaded=%d duplicates=%d conflicts=%d errors=%d",
				report.Uploaded, report.Downloaded, report.Duplicates, report.Conflicts, len(report.Errors))
		}
		if *interval <= 0 {
			return
		}
		time.Sleep(*interval)
	}
}

func nodeCmd(args []string) {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	dbPath := fs.String("db", config.DefaultDBPath(), "SQLite database path")
	listen := fs.String("listen", "127.0.0.1:8787", "listen address")
	token := fs.String("token", os.Getenv("TM_API_TOKEN"), "API token")
	deviceName := fs.String("device-name", "", "device display name")
	collectInterval := fs.Duration("collect-interval", 5*time.Minute, "local collection interval")
	upstream := fs.String("upstream", os.Getenv("TM_UPSTREAM"), "upstream server URL")
	upstreamToken := fs.String("upstream-token", os.Getenv("TM_UPSTREAM_TOKEN"), "upstream token")
	webdav := fs.String("webdav", os.Getenv("TM_WEBDAV_URL"), "WebDAV URL")
	webdavUser := fs.String("webdav-user", os.Getenv("TM_WEBDAV_USER"), "WebDAV username")
	webdavPassword := fs.String("webdav-password", os.Getenv("TM_WEBDAV_PASSWORD"), "WebDAV password")
	fs.Parse(args)
	db, err := openDB(*dbPath)
	must(err)
	defer db.Close()
	identity, err := identityFor(*dbPath, *deviceName)
	must(err)
	must(registerIdentity(db, identity))
	state := &server.AppState{DB: db, Token: *token, Version: version}
	httpServer := &http.Server{Addr: *listen, Handler: server.NewRouter(state), ReadHeaderTimeout: 10 * time.Second}
	go func() { log.Printf("HTTP server listening on %s", *listen); must(httpServer.ListenAndServe()) }()

	cfg := syncpkg.DefaultConfig()
	if *upstream != "" {
		cfg.Mode = syncpkg.ModeServer
		cfg.Server = syncpkg.Server{BaseURL: *upstream, Token: *upstreamToken}
	} else if *webdav != "" {
		cfg.Mode = syncpkg.ModeWebDAV
		cfg.WebDAV = syncpkg.WebDAV{BaseURL: *webdav, Username: *webdavUser, Password: *webdavPassword,
			RemoteRoot: "tokenanalyse", Profile: "default"}
	}
	runner := syncpkg.NewRunner(db, cfg, identity.DeviceID)
	go func() {
		ticker := time.NewTicker(*collectInterval)
		defer ticker.Stop()
		for {
			result := collectOnce(db, identity)
			log.Printf("collection files=%d events=%d errors=%d", result.FilesScanned, len(result.Events), len(result.Errors))
			<-ticker.C
		}
	}()
	go func() {
		if cfg.Mode == syncpkg.ModeDisabled {
			return
		}
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		for {
			report, err := runner.RunOnce(context.Background())
			if err != nil {
				log.Printf("sync error: %v", err)
			} else {
				log.Printf("sync uploaded=%d downloaded=%d", report.Uploaded, report.Downloaded)
			}
			<-ticker.C
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}

func summaryCmd(args []string) {
	fs := flag.NewFlagSet("summary", flag.ExitOnError)
	dbPath := fs.String("db", config.DefaultDBPath(), "SQLite database path")
	days := fs.Int("days", 7, "time range in days")
	deviceID := fs.String("device", "", "device ID")
	appType := fs.String("app", "", "application type")
	fs.Parse(args)
	db, err := openDB(*dbPath)
	must(err)
	defer db.Close()
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -*days)
	summary, err := db.GetSummary(model.UsageFilters{From: &from, To: &now, DeviceID: *deviceID, AppType: *appType})
	must(err)
	writeJSON(summary)
}

func devicesCmd(args []string) {
	fs := flag.NewFlagSet("devices", flag.ExitOnError)
	dbPath := fs.String("db", config.DefaultDBPath(), "SQLite database path")
	fs.Parse(args)
	db, err := openDB(*dbPath)
	must(err)
	defer db.Close()
	stats, err := db.GetDeviceStats(model.UsageFilters{})
	must(err)
	writeJSON(stats)
}

func pricingCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "pricing requires: show, sync-models-dev, import, reprice, aliases")
		return
	}
	fs := flag.NewFlagSet("pricing", flag.ExitOnError)
	dbPath := fs.String("db", config.DefaultDBPath(), "SQLite database path")
	file := fs.String("file", "", "custom pricing JSON file")
	aliasPattern := fs.String("alias", "", "alias pattern")
	canonical := fs.String("canonical", "", "canonical model ID")
	matchType := fs.String("match-type", "exact", "exact, prefix, regex")
	scopeApp := fs.String("app", "", "application scope")
	aliasID := fs.String("id", "", "alias ID to delete")
	fs.Parse(args[1:])
	db, err := openDB(*dbPath)
	must(err)
	defer db.Close()
	switch args[0] {
	case "show":
		snapshot, err := db.ActivePricingSnapshot()
		must(err)
		writeJSON(snapshot)
	case "sync-models-dev":
		snapshot, err := pricing.FetchModelsDev(context.Background())
		must(err)
		must(db.ApplyPricingSnapshot(storage.PricingSnapshot{VersionID: snapshot.VersionID, Source: snapshot.Source,
			CreatedAt: snapshot.CreatedAt, Models: snapshot.Models, Aliases: snapshot.Aliases, ContentHash: snapshot.ContentHash}, true))
		log.Printf("models.dev pricing synced: %d models", len(snapshot.Models))
	case "import":
		if *file == "" {
			must(fmt.Errorf("--file is required"))
		}
		data, err := os.ReadFile(*file)
		must(err)
		snapshot, err := pricing.LoadCustom(data, "custom")
		must(err)
		must(db.ApplyPricingSnapshot(storage.PricingSnapshot{VersionID: snapshot.VersionID, Source: snapshot.Source,
			CreatedAt: snapshot.CreatedAt, Models: snapshot.Models, Aliases: snapshot.Aliases, ContentHash: snapshot.ContentHash}, true))
		log.Printf("custom pricing imported: %d models", len(snapshot.Models))
	case "reprice":
		count, err := db.RepriceAll()
		must(err)
		log.Printf("repriced %d events", count)
	case "aliases":
		if *aliasPattern != "" && *canonical != "" {
			must(db.UpsertModelAlias(model.ModelAlias{AliasPattern: *aliasPattern, CanonicalModelID: *canonical,
				MatchType: *matchType, ScopeApp: *scopeApp, Priority: 100}))
			return
		}
		if *aliasID != "" {
			must(db.DeleteModelAlias(*aliasID))
			return
		}
		aliases, err := db.ListModelAliases()
		must(err)
		writeJSON(aliases)
	default:
		must(fmt.Errorf("unknown pricing command: %s", args[0]))
	}
}

func importCCCmd(args []string) {
	fs := flag.NewFlagSet("import-cc-switch", flag.ExitOnError)
	source := fs.String("source", "", "CC Switch SQLite database")
	dbPath := fs.String("db", config.DefaultDBPath(), "TokenAnalyse SQLite database")
	deviceName := fs.String("device-name", "", "device display name")
	fs.Parse(args)
	if *source == "" {
		must(fmt.Errorf("--source is required"))
	}
	db, err := openDB(*dbPath)
	must(err)
	defer db.Close()
	identity, err := identityFor(*dbPath, *deviceName)
	must(err)
	must(registerIdentity(db, identity))
	result, err := collectors.ImportCCSwitch(*source, identity.DeviceID, 0)
	must(err)
	inserted, err := db.InsertEvents(result.Events, false)
	must(err)
	log.Printf("CC Switch import scanned=%d inserted=%d skipped=%d", result.FilesScanned, inserted.Inserted, inserted.Skipped)
}

func writeJSON(value any) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	must(encoder.Encode(value))
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
