package ais

import (
	"context"
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// defaultCodexModel is used on the codex backend when no model is saved or
// passed with -m. ais makes small, latency-sensitive calls, so it defaults to
// the fast tier rather than inheriting the (often heavier) model from the user's
// own Codex config.
const defaultCodexModel = "gpt-6-luna"

// modelInfo is one entry of the Codex model catalog, trimmed to what ais shows.
type modelInfo struct {
	Slug          string
	Description   string
	Efforts       []string
	DefaultEffort string
	Notice        string // e.g. a retirement / migration notice
}

// fallbackCodexModels is used when the live catalog is unavailable (older Codex
// without `codex debug models`, or the command fails).
var fallbackCodexModels = []modelInfo{
	{Slug: "gpt-6-luna", Description: "Fast and affordable model for easier tasks.", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "medium"},
	{Slug: "gpt-6-sol", Description: "Workhorse model for coding and everyday work.", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "medium"},
	{Slug: "gpt-6-astra", Description: "Frontier intelligence for the most demanding work.", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "medium"},
}

// loadCodexModels returns the models the logged-in account can pick, in Codex's
// own display order, read from `codex debug models`. The bool reports whether the
// list is live (true) or the built-in fallback (false).
func loadCodexModels(codexBin string) ([]modelInfo, bool) {
	if codexBin == "" {
		return fallbackCodexModels, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, codexBin, "debug", "models").Output()
	if err != nil {
		return fallbackCodexModels, false
	}
	models := parseCodexModels(raw)
	if len(models) == 0 {
		return fallbackCodexModels, false
	}
	return models, true
}

func parseCodexModels(raw []byte) []modelInfo {
	var catalog struct {
		Models []struct {
			Slug             string `json:"slug"`
			Description      string `json:"description"`
			Visibility       string `json:"visibility"`
			Priority         int    `json:"priority"`
			DefaultReasoning string `json:"default_reasoning_level"`
			Reasoning        []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
			Upgrade *struct {
				Migration string `json:"migration_markdown"`
			} `json:"upgrade"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil
	}
	sort.SliceStable(catalog.Models, func(i, j int) bool {
		return catalog.Models[i].Priority < catalog.Models[j].Priority
	})
	models := make([]modelInfo, 0, len(catalog.Models))
	for _, m := range catalog.Models {
		// "hide" entries are internal (auto-review, reserved) and not user-pickable.
		if m.Slug == "" || m.Visibility != "list" {
			continue
		}
		info := modelInfo{Slug: m.Slug, Description: strings.TrimSpace(m.Description), DefaultEffort: m.DefaultReasoning}
		for _, r := range m.Reasoning {
			if r.Effort != "" {
				info.Efforts = append(info.Efforts, r.Effort)
			}
		}
		if m.Upgrade != nil {
			info.Notice = strings.TrimSpace(m.Upgrade.Migration)
		}
		models = append(models, info)
	}
	return models
}

func findModel(models []modelInfo, slug string) (modelInfo, bool) {
	for _, m := range models {
		if m.Slug == slug {
			return m, true
		}
	}
	return modelInfo{}, false
}
