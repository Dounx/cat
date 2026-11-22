package engine

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/dounx/cat/core/rules"
	"github.com/dounx/cat/core/store"
)

const (
	BackupFormat  = "cat-backup"
	BackupVersion = 1
	SettingsScope = "settings"
)

// BackupSettings holds workspace-level hub settings included in a backup.
type BackupSettings struct {
	PublicURL string `json:"public_url,omitempty"`
	MarketURL string `json:"market_url,omitempty"`
}

// BackupInstance is an exported instance record with secrets redacted.
type BackupInstance struct {
	store.Instance
	// MaskedSecrets lists config fields whose secret values were replaced by
	// SecretMask during export and must be re-entered on a new machine.
	MaskedSecrets []string `json:"masked_secrets,omitempty"`
}

// WorkspaceBackup is a portable, versioned snapshot of workspace settings,
// redacted instances, and rules.
type WorkspaceBackup struct {
	Format     string           `json:"format"`
	Version    int              `json:"version"`
	ExportedAt time.Time        `json:"exported_at"`
	Settings   BackupSettings   `json:"settings"`
	Instances  []BackupInstance `json:"instances"`
	Rules      []rules.Rule     `json:"rules"`
}

// ImportResult summarizes what a workspace import changed and what still
// requires user attention.
type ImportResult struct {
	InstancesImported int      `json:"instances_imported"`
	InstancesSkipped  int      `json:"instances_skipped"`
	RulesImported     int      `json:"rules_imported"`
	RulesSkipped      int      `json:"rules_skipped"`
	SettingsUpdated   bool     `json:"settings_updated"`
	Warnings          []string `json:"warnings"`
	NeedsSecrets      []string `json:"needs_secrets"`
}

// NormalizePublicURL validates and normalizes a hub public URL.
func NormalizePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("对外地址要以 http:// 或 https:// 开头，后面跟域名或 IP")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("对外地址里不要带 ?、# 或用户名密码")
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// NormalizeMarketURL validates and normalizes a plugin market URL.
func NormalizeMarketURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.EqualFold(raw, "off") {
		return "off", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("插件市场地址格式不对")
	}
	if u.Scheme == "file" && u.Path != "" {
		return raw, nil
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil {
		return raw, nil
	}
	return "", errors.New("插件市场地址要以 http://、https:// 或 file:// 开头，或者填 off 关闭")
}

// ExportBackup builds a redacted workspace bundle suitable for backup and
// cross-machine migration. Secret values are never exported.
func (e *Engine) ExportBackup() (WorkspaceBackup, error) {
	pubURL, _ := e.Store.KVGet(SettingsScope, "public_url")
	mktURL, _ := e.Store.KVGet(SettingsScope, "market_url")

	storedInstances, err := e.Store.ListInstances()
	if err != nil {
		return WorkspaceBackup{}, err
	}
	storedRules, err := e.Store.ListRules()
	if err != nil {
		return WorkspaceBackup{}, err
	}

	instances := make([]BackupInstance, 0, len(storedInstances))
	for _, rec := range storedInstances {
		if m, ok := e.Plugin(rec.Plugin); ok && m.Singleton {
			continue
		}
		maskedRec := e.masked(rec)
		if maskedRec.Config == nil {
			maskedRec.Config = map[string]any{}
		}
		secretSet := map[string]bool{}
		for _, f := range e.secretFields(rec.Plugin) {
			if s, _ := maskedRec.Config[f].(string); s == SecretMask {
				secretSet[f] = true
			}
		}
		for k, v := range rec.Config {
			if v == secretRef || v == SecretMask {
				maskedRec.Config[k] = SecretMask
				secretSet[k] = true
			}
		}
		var maskedFields []string
		for f := range secretSet {
			maskedFields = append(maskedFields, f)
		}
		sort.Strings(maskedFields)
		instances = append(instances, BackupInstance{
			Instance:      maskedRec,
			MaskedSecrets: maskedFields,
		})
	}
	if storedRules == nil {
		storedRules = []rules.Rule{}
	}
	return WorkspaceBackup{
		Format:     BackupFormat,
		Version:    BackupVersion,
		ExportedAt: time.Now().UTC(),
		Settings: BackupSettings{
			PublicURL: pubURL,
			MarketURL: mktURL,
		},
		Instances: instances,
		Rules:     storedRules,
	}, nil
}

// ImportBackup restores settings, instances, and rules from a WorkspaceBackup.
// mode must be "merge" (default when empty) or "replace".
func (e *Engine) ImportBackup(backup WorkspaceBackup, mode string) (ImportResult, error) {
	res := ImportResult{
		Warnings:     []string{},
		NeedsSecrets: []string{},
	}
	if mode == "" {
		mode = "merge"
	}
	if mode != "merge" && mode != "replace" {
		return res, fmt.Errorf("不支持的导入模式 %q（仅支持 merge 或 replace）", mode)
	}
	if backup.Format != BackupFormat {
		return res, fmt.Errorf("不是有效的 Cat 工作区备份（format=%q）", backup.Format)
	}
	if backup.Version != BackupVersion {
		return res, fmt.Errorf("不支持的备份版本 %d（当前支持版本 %d）", backup.Version, BackupVersion)
	}

	pubURL, err := NormalizePublicURL(backup.Settings.PublicURL)
	if err != nil {
		return res, err
	}
	mktURL, err := NormalizeMarketURL(backup.Settings.MarketURL)
	if err != nil {
		return res, err
	}

	// Restore settings.
	curPub, _ := e.Store.KVGet(SettingsScope, "public_url")
	curMkt, _ := e.Store.KVGet(SettingsScope, "market_url")
	if mode == "replace" {
		if pubURL == "" {
			if err := e.Store.KVDelete(SettingsScope, "public_url"); err != nil {
				return res, err
			}
		} else if err := e.Store.KVSet(SettingsScope, "public_url", pubURL); err != nil {
			return res, err
		}
		if mktURL == "" {
			if err := e.Store.KVDelete(SettingsScope, "market_url"); err != nil {
				return res, err
			}
		} else if err := e.Store.KVSet(SettingsScope, "market_url", mktURL); err != nil {
			return res, err
		}
		if curPub != pubURL || curMkt != mktURL {
			res.SettingsUpdated = true
		}
	} else {
		if pubURL != "" && pubURL != curPub {
			if err := e.Store.KVSet(SettingsScope, "public_url", pubURL); err != nil {
				return res, err
			}
			res.SettingsUpdated = true
		}
		if mktURL != "" && mktURL != curMkt {
			if err := e.Store.KVSet(SettingsScope, "market_url", mktURL); err != nil {
				return res, err
			}
			res.SettingsUpdated = true
		}
	}

	storedInstances, err := e.Store.ListInstances()
	if err != nil {
		return res, err
	}
	existingInsts := make(map[string]store.Instance, len(storedInstances))
	for _, rec := range storedInstances {
		existingInsts[rec.ID] = rec
	}

	storedRules, err := e.Store.ListRules()
	if err != nil {
		return res, err
	}
	existingRules := make(map[string]rules.Rule, len(storedRules))
	for _, r := range storedRules {
		existingRules[r.ID] = r
	}

	if mode == "replace" {
		backupInstIDs := map[string]bool{}
		for _, bi := range backup.Instances {
			if bi.ID != "" {
				backupInstIDs[bi.ID] = true
			}
		}
		for _, rec := range storedInstances {
			if m, ok := e.Plugin(rec.Plugin); ok && m.Singleton {
				continue
			}
			// Keep instances that will be overwritten in place so existing
			// stored secrets on the same machine remain intact when masked.
			if backupInstIDs[rec.ID] {
				continue
			}
			if err := e.DeleteInstance(rec.ID); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("清理旧接入「%s」失败：%v", rec.Name, err))
			} else {
				delete(existingInsts, rec.ID)
			}
		}
		for _, r := range storedRules {
			if err := e.Store.DeleteRule(r.ID); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("清理旧规则「%s」失败：%v", r.Name, err))
			} else {
				delete(existingRules, r.ID)
			}
		}
		if err := e.reloadRules(); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("刷新规则失败：%v", err))
		}
	}

	needsSeen := map[string]bool{}
	for _, bi := range backup.Instances {
		name := bi.Name
		if name == "" {
			name = bi.ID
		}
		if name == "" {
			name = bi.Plugin
		}
		if m, ok := e.Plugin(bi.Plugin); ok && m.Singleton {
			continue
		}
		if mode == "merge" && bi.ID != "" {
			if _, exists := existingInsts[bi.ID]; exists {
				res.InstancesSkipped++
				continue
			}
		}
		if _, ok := e.Plugin(bi.Plugin); !ok {
			res.Warnings = append(res.Warnings, fmt.Sprintf("接入「%s」依赖未安装的插件 %q，已跳过", name, bi.Plugin))
			res.InstancesSkipped++
			continue
		}

		// Determine if any masked secret field lacks a local stored secret.
		maskedFieldSet := map[string]bool{}
		for _, f := range bi.MaskedSecrets {
			maskedFieldSet[f] = true
		}
		for _, f := range e.secretFields(bi.Plugin) {
			if s, _ := bi.Config[f].(string); s == SecretMask || s == secretRef {
				maskedFieldSet[f] = true
			}
		}
		missingSecret := false
		for f := range maskedFieldSet {
			hasLocal := false
			if e.Secrets != nil && bi.ID != "" {
				_, hasLocal = e.Secrets.Get(bi.ID + "/" + f)
			}
			if !hasLocal {
				missingSecret = true
				break
			}
		}

		saved, err := e.SaveInstance(bi.Instance)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("接入「%s」导入失败：%v", name, err))
			res.InstancesSkipped++
			continue
		}
		existingInsts[saved.ID] = saved
		res.InstancesImported++
		if missingSecret && !needsSeen[saved.Name] {
			needsSeen[saved.Name] = true
			res.NeedsSecrets = append(res.NeedsSecrets, saved.Name)
		}
	}

	for _, r := range backup.Rules {
		if mode == "merge" && r.ID != "" {
			if _, exists := existingRules[r.ID]; exists {
				res.RulesSkipped++
				continue
			}
		}
		saved, err := e.SaveRule(r)
		if err != nil {
			label := r.Name
			if label == "" {
				label = r.ID
			}
			res.Warnings = append(res.Warnings, fmt.Sprintf("规则「%s」导入失败：%v", label, err))
			res.RulesSkipped++
			continue
		}
		existingRules[saved.ID] = saved
		res.RulesImported++
	}

	e.Publish("rules", map[string]any{"changed": true})
	return res, nil
}
