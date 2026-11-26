package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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
	if err := r.db.RetryFailedOutbox(); err != nil {
		return report, err
	}

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
				if markErr := r.db.MarkOutboxFailed(entry.OutboxID, err.Error()); markErr != nil {
					report.Errors = append(report.Errors, markErr.Error())
				}
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
				if markErr := r.db.MarkOutboxFailed(id, err.Error()); markErr != nil {
					report.Errors = append(report.Errors, markErr.Error())
				}
			}
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		report.Uploaded += ack.Accepted
		report.Duplicates += ack.Duplicates
		for _, id := range outboxIDs {
			if err := r.db.MarkOutboxSent(id); err != nil {
				report.Errors = append(report.Errors, err.Error())
			}
		}
	}

	cursor, err := r.db.GetSetting("upstream_cursor")
	if err != nil {
		return report, err
	}
	remoteEvents, err := client.FetchEvents(ctx, cursor, 1000)
	if err != nil {
		report.Errors = append(report.Errors, "fetch events: "+err.Error())
	} else {
		imported := true
		if len(remoteEvents.Events) > 0 {
			result, err := r.db.InsertEvents(remoteEvents.Events, false)
			if err != nil {
				imported = false
				report.Errors = append(report.Errors, "import events: "+err.Error())
			} else {
				report.Downloaded += result.Inserted
				report.Duplicates += result.Skipped
			}
		}
		if imported && remoteEvents.NextCursor != "" {
			if err := r.db.SetSetting("upstream_cursor", remoteEvents.NextCursor); err != nil {
				report.Errors = append(report.Errors, "save upstream cursor: "+err.Error())
			}
		}
	}

	remotePricing, err := client.FetchPricing(ctx)
	checkedAt := time.Now().UTC()
	if err != nil {
		remoteHash := r.currentPricingRemoteHash()
		message := "fetch pricing: " + err.Error()
		r.markPricingSync(&report, remoteHash, checkedAt, time.Time{}, message)
		report.Errors = append(report.Errors, message)
	} else if !hasPricingContent(remotePricing) {
		r.markPricingSync(&report, r.currentPricingRemoteHash(), checkedAt, time.Time{}, "")
	} else {
		remoteHash := pricingSnapshotHash(remotePricing)
		localPricing, err := r.db.ActivePricingSnapshot()
		if err != nil {
			message := "load local pricing: " + err.Error()
			r.markPricingSync(&report, remoteHash, checkedAt, time.Time{}, message)
			report.Errors = append(report.Errors, message)
		} else if pricingSnapshotsDiffer(localPricing, remotePricing) {
			_, conflicts, mergeErr := r.db.MergePricingSnapshot(remotePricing, pricing.MergePreferRemote)
			if mergeErr != nil {
				r.markPricingSync(&report, remoteHash, checkedAt, time.Time{}, "merge pricing: "+mergeErr.Error())
				report.Errors = append(report.Errors, "merge pricing: "+mergeErr.Error())
			} else {
				r.markPricingSync(&report, remoteHash, checkedAt, checkedAt, "")
				report.Conflicts += conflicts
			}
		} else {
			r.markPricingSync(&report, remoteHash, checkedAt, checkedAt, "")
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
	if err := r.db.RetryFailedOutbox(); err != nil {
		return report, err
	}
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
			if markErr := r.db.MarkOutboxFailed(entry.OutboxID, err.Error()); markErr != nil {
				report.Errors = append(report.Errors, markErr.Error())
			}
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		if err := r.db.MarkOutboxSent(entry.OutboxID); err != nil {
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		report.Uploaded++
	}

	ownPricing, err := r.db.ActivePricingSnapshot()
	pricingErrors := []string{}
	if err != nil {
		pricingErrors = append(pricingErrors, "load local pricing: "+err.Error())
	}
	pricingSynced := false
	remoteHash := ""
	ownDeviceURL := client.DeviceURL(root, profile, r.deviceID)
	if ownPricing.ContentHash != "" {
		if err := client.EnsureDir(ctx, ownDeviceURL); err != nil {
			pricingErrors = append(pricingErrors, err.Error())
		} else if payload, err := json.Marshal(ownPricing); err != nil {
			pricingErrors = append(pricingErrors, err.Error())
		} else if err := client.Put(ctx, ownDeviceURL+"pricing.json", payload, "application/json"); err != nil {
			pricingErrors = append(pricingErrors, err.Error())
		} else {
			pricingSynced = true
		}
	}

	deviceNames, err := client.ListChildren(ctx, devicesURL)
	if err != nil {
		pricingErrors = append(pricingErrors, err.Error())
		r.finishWebDAVPricingSync(&report, remoteHash, pricingSynced, pricingErrors)
		return report, nil
	}
	pricingChanged := false
	for _, deviceID := range deviceNames {
		if deviceID == r.deviceID || deviceID == "devices" {
			continue
		}
		deviceURL := client.DeviceURL(root, profile, deviceID)
		if data, found, err := client.Get(ctx, deviceURL+"pricing.json"); err != nil {
			pricingErrors = append(pricingErrors, err.Error())
		} else if found {
			var remotePricing storage.PricingSnapshot
			if err := json.Unmarshal(data, &remotePricing); err != nil {
				pricingErrors = append(pricingErrors, err.Error())
			} else if hasPricingContent(remotePricing) {
				remoteHash = pricingSnapshotHash(remotePricing)
				localPricing, err := r.db.ActivePricingSnapshot()
				if err != nil {
					pricingErrors = append(pricingErrors, err.Error())
				} else if pricingSnapshotsDiffer(localPricing, remotePricing) {
					merged, conflicts, mergeErr := r.db.MergePricingSnapshot(remotePricing, pricing.MergePreferRemote)
					if mergeErr != nil {
						pricingErrors = append(pricingErrors, "merge pricing: "+mergeErr.Error())
					} else {
						ownPricing = merged
						if merged.VersionID != localPricing.VersionID {
							pricingChanged = true
						}
						pricingSynced = true
						report.Conflicts += conflicts
					}
				} else {
					ownPricing = localPricing
					pricingSynced = true
				}
			}
		}
		batchesURL := deviceURL + "batches/"
		names, err := client.ListChildren(ctx, batchesURL)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		for _, name := range names {
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			data, found, err := client.Get(ctx, batchesURL+url.PathEscape(name))
			if err != nil {
				report.Errors = append(report.Errors, err.Error())
				continue
			}
			if !found {
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
	if pricingChanged && ownPricing.ContentHash != "" {
		payload, err := json.Marshal(ownPricing)
		if err != nil {
			pricingErrors = append(pricingErrors, err.Error())
		} else if err := client.Put(ctx, ownDeviceURL+"pricing.json", payload, "application/json"); err != nil {
			pricingErrors = append(pricingErrors, err.Error())
		}
	}
	r.finishWebDAVPricingSync(&report, remoteHash, pricingSynced, pricingErrors)
	return report, nil
}

func (r *Runner) finishWebDAVPricingSync(report *Report, remoteHash string, synced bool, errs []string) {
	for _, err := range errs {
		if err != "" {
			report.Errors = append(report.Errors, err)
		}
	}
	checkedAt := time.Now().UTC()
	if remoteHash == "" {
		remoteHash = r.currentPricingRemoteHash()
	}
	var syncedAt time.Time
	if synced {
		syncedAt = checkedAt
	}
	r.markPricingSync(report, remoteHash, checkedAt, syncedAt, strings.Join(errs, "; "))
}

func (r *Runner) currentPricingRemoteHash() string {
	status, err := r.db.PricingSyncStatus()
	if err != nil {
		return ""
	}
	return status.RemoteHash
}

func (r *Runner) markPricingSync(report *Report, remoteHash string, checkedAt, syncedAt time.Time, syncError string) {
	if err := r.db.MarkPricingSync(remoteHash, checkedAt, syncedAt, syncError); err != nil {
		report.Errors = append(report.Errors, "save pricing sync metadata: "+err.Error())
	}
}

func hasPricingContent(snapshot storage.PricingSnapshot) bool {
	return snapshot.VersionID != "" || snapshot.ContentHash != "" || len(snapshot.Models) > 0 || len(snapshot.Aliases) > 0
}

func pricingSnapshotHash(snapshot storage.PricingSnapshot) string {
	if snapshot.ContentHash != "" {
		return snapshot.ContentHash
	}
	return pricing.HashModels(snapshot.Models)
}

func pricingSnapshotsDiffer(local, remote storage.PricingSnapshot) bool {
	return pricing.HashModels(local.Models) != pricing.HashModels(remote.Models) ||
		pricing.HashAliases(local.Aliases) != pricing.HashAliases(remote.Aliases)
}
