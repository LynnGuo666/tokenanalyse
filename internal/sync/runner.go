package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"tokenanalyse/internal/model"
	"tokenanalyse/internal/pricing"
	"tokenanalyse/internal/storage"
)

type Runner struct {
	db       *storage.DB
	config   Config
	deviceID string
}

func NewRunner(db *storage.DB, config Config, deviceID string) *Runner {
	return &Runner{db: db, config: config, deviceID: deviceID}
}

func nowUTC() time.Time { return time.Now().UTC() }

func (r *Runner) RunOnce(ctx context.Context) (Report, error) {
	report := Report{}
	switch r.config.Mode {
	case ModeServer:
		return r.runServer(ctx)
	case ModeWebDAV:
		return r.runWebDAV(ctx)
	default:
		return report, nil
	}
}

func (r *Runner) runServer(ctx context.Context) (Report, error) {
	report := Report{}
	client := NewServerClient(r.config.Server.BaseURL, r.config.Server.Token)
	_ = r.db.RetryFailedOutbox()

	entries, err := r.db.PendingOutbox(1000)
	if err != nil {
		return report, err
	}
	groups := map[string][]storage.OutboxEntry{}
	for _, entry := range entries {
		if entry.EventType != "usage_event" {
			continue
		}
		groups[entry.DeviceID] = append(groups[entry.DeviceID], entry)
	}
	deviceIDs := make([]string, 0, len(groups))
	for deviceID := range groups {
		deviceIDs = append(deviceIDs, deviceID)
	}
	sort.Strings(deviceIDs)
	for _, deviceID := range deviceIDs {
		events := make([]model.UsageEvent, 0, len(groups[deviceID]))
		outboxIDs := make([]int64, 0, len(groups[deviceID]))
		for _, entry := range groups[deviceID] {
			event, err := entry.Event()
			if err != nil {
				report.Errors = append(report.Errors, err.Error())
				continue
			}
			events = append(events, event)
			outboxIDs = append(outboxIDs, entry.OutboxID)
		}
		if len(events) == 0 {
			continue
		}
		cursor := outboxIDs[len(outboxIDs)-1]
		ack, err := client.PushBatch(ctx, EventBatch{BatchID: fmt.Sprintf("%s-%d", r.deviceID, cursor),
			DeviceID: deviceID, CreatedAt: nowUTC(), Cursor: cursor, Events: events})
		if err != nil {
			for _, id := range outboxIDs {
				_ = r.db.MarkOutboxFailed(id, err.Error())
			}
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		report.Uploaded += ack.Accepted
		report.Duplicates += ack.Duplicates
		for _, id := range outboxIDs {
			_ = r.db.MarkOutboxSent(id)
		}
	}

	cursor, _ := r.db.GetSetting("upstream_cursor")
	remoteEvents, err := client.FetchEvents(ctx, cursor, 1000)
	if err != nil {
		report.Errors = append(report.Errors, "fetch events: "+err.Error())
	} else {
		if len(remoteEvents.Events) > 0 {
			result, err := r.db.InsertEvents(remoteEvents.Events, false)
			if err != nil {
				report.Errors = append(report.Errors, "import events: "+err.Error())
			} else {
				report.Downloaded += result.Inserted
				report.Duplicates += result.Skipped
			}
		}
		if remoteEvents.NextCursor != "" {
			_ = r.db.SetSetting("upstream_cursor", remoteEvents.NextCursor)
		}
	}

	remotePricing, err := client.FetchPricing(ctx)
	checkedAt := time.Now().UTC()
	if err != nil {
		_ = r.db.MarkPricingSync("", checkedAt, time.Time{}, err.Error())
		report.Errors = append(report.Errors, "fetch pricing: "+err.Error())
	} else {
		localPricing, _ := r.db.ActivePricingSnapshot()
		if remotePricing.ContentHash != "" && remotePricing.ContentHash != localPricing.ContentHash {
			_, conflicts, mergeErr := r.db.MergePricingSnapshot(remotePricing, pricing.MergePreferRemote)
			if mergeErr != nil {
				_ = r.db.MarkPricingSync(remotePricing.ContentHash, checkedAt, time.Time{}, mergeErr.Error())
				report.Errors = append(report.Errors, "merge pricing: "+mergeErr.Error())
			} else {
				_ = r.db.MarkPricingSync(remotePricing.ContentHash, checkedAt, checkedAt, "")
				report.Conflicts += conflicts
			}
		} else {
			_ = r.db.MarkPricingSync(remotePricing.ContentHash, checkedAt, checkedAt, "")
		}
	}
	return report, nil
}

func (r *Runner) runWebDAV(ctx context.Context) (Report, error) {
	report := Report{}
	client := NewWebDAVClient(r.config.WebDAV.BaseURL, r.config.WebDAV.Username, r.config.WebDAV.Password)
	root, profile := r.config.WebDAV.RemoteRoot, r.config.WebDAV.Profile
	devicesURL := client.UsageDevicesURL(root, profile)
	if err := client.EnsureDir(ctx, devicesURL); err != nil {
		return report, err
	}
	_ = r.db.RetryFailedOutbox()
	entries, err := r.db.PendingOutbox(1000)
	if err != nil {
		return report, err
	}
	for _, entry := range entries {
		if entry.EventType != "usage_event" {
			continue
		}
		deviceURL := client.DeviceURL(root, profile, entry.DeviceID)
		batchesURL := deviceURL + "batches/"
		if err := client.EnsureDir(ctx, deviceURL); err != nil {
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		if err := client.EnsureDir(ctx, batchesURL); err != nil {
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		objectURL := batchesURL + fmt.Sprintf("%020d-%s.json", entry.OutboxID, entry.EventID)
		if err := client.Put(ctx, objectURL, []byte(entry.Payload), "application/json"); err != nil {
			_ = r.db.MarkOutboxFailed(entry.OutboxID, err.Error())
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		_ = r.db.MarkOutboxSent(entry.OutboxID)
		report.Uploaded++
	}

	deviceNames, err := client.ListChildren(ctx, devicesURL)
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
		return report, nil
	}
	for _, deviceID := range deviceNames {
		if deviceID == r.deviceID || deviceID == "devices" {
			continue
		}
		batchesURL := client.DeviceURL(root, profile, deviceID) + "batches/"
		names, err := client.ListChildren(ctx, batchesURL)
		if err != nil {
			continue
		}
		for _, name := range names {
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			data, found, err := client.Get(ctx, batchesURL+name)
			if err != nil || !found {
				continue
			}
			var event model.UsageEvent
			if err := json.Unmarshal(data, &event); err != nil {
				report.Errors = append(report.Errors, err.Error())
				continue
			}
			result, err := r.db.InsertEvents([]model.UsageEvent{event}, false)
			if err != nil {
				report.Errors = append(report.Errors, err.Error())
				continue
			}
			report.Downloaded += result.Inserted
			report.Duplicates += result.Skipped
		}
	}
	return report, nil
}
