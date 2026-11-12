// Package engine is the event hub itself: it owns plugin instances, receives
// their events, matches rules and runs action pipelines.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/id"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/rules"
	"github.com/dounx/cat/core/store"
)

const (
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusStopped  = "stopped"
	StatusError    = "error"

	defaultStepTimeout = 10 * time.Minute
	keepEvents         = 20000
)

// Message is pushed to live subscribers (the UI's event stream).
type Message struct {
	Kind string `json:"kind"` // event | run | instance | log
	Data any    `json:"data"`
}

// InstanceView is an instance plus its live status.
type InstanceView struct {
	store.Instance
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// RuleView is a rule plus the last problem evaluating it, if any.
type RuleView struct {
	rules.Rule
	Error string `json:"error,omitempty"`
}

// LogEntry is a line logged by an instance.
type LogEntry struct {
	Instance string    `json:"instance"`
	Level    string    `json:"level"`
	Message  string    `json:"message"`
	Time     time.Time `json:"time"`
}

type registered struct {
	manifest plugin.Manifest
	factory  plugin.Factory
}

type running struct {
	rec    store.Instance
	inst   plugin.Instance
	cancel context.CancelFunc
	status string
	err    string
	// what the rules ask this instance to watch, as last handed to it
	triggers []plugin.Trigger
}

type Engine struct {
	Store *store.Store
	// Secrets, when set, holds credential config values instead of the database.
	Secrets Secrets
	dataDir string
	log     *slog.Logger

	mu       sync.RWMutex
	plugins  map[string]registered
	running  map[string]*running
	rules    []*rules.Compiled
	ruleErrs map[string]string
	throttle map[string]time.Time
	// trigErrs holds what each instance found wrong with its triggers, by
	// instance id and then trigger id.
	trigErrs map[string]map[string]string
	// trigMu orders the handing over of triggers to instances.
	trigMu sync.Mutex

	subMu sync.Mutex
	subs  map[chan Message]struct{}

	sem     chan struct{}
	wg      sync.WaitGroup
	emitted int
}

func New(st *store.Store, dataDir string, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		Store: st, dataDir: dataDir, log: log,
		plugins:  map[string]registered{},
		running:  map[string]*running{},
		ruleErrs: map[string]string{},
		throttle: map[string]time.Time{},
		trigErrs: map[string]map[string]string{},
		subs:     map[chan Message]struct{}{},
		sem:      make(chan struct{}, 16),
	}
}

func (e *Engine) DataDir() string { return e.dataDir }

// ---- plugins ----

// RegisterPlugin makes a plugin available. Re-registering replaces it.
func (e *Engine) RegisterPlugin(m plugin.Manifest, f plugin.Factory) {
	if c, ok := plugin.NormalizeCategory(m.Category); ok {
		m.Category = c
	} else {
		m.Category = plugin.CategoryChannel
	}
	e.mu.Lock()
	e.plugins[m.ID] = registered{m, f}
	e.mu.Unlock()
}

func (e *Engine) UnregisterPlugin(pluginID string) {
	e.mu.Lock()
	delete(e.plugins, pluginID)
	e.mu.Unlock()
}

func (e *Engine) Plugins() []plugin.Manifest {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]plugin.Manifest, 0, len(e.plugins))
	for _, p := range e.plugins {
		out = append(out, p.manifest)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Builtin != out[j].Builtin {
			return out[i].Builtin
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (e *Engine) Plugin(pluginID string) (plugin.Manifest, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	p, ok := e.plugins[pluginID]
	return p.manifest, ok
}

// ---- lifecycle ----

// Start loads rules and starts every enabled instance.
func (e *Engine) Start() error {
	if err := e.reloadRules(); err != nil {
		return err
	}
	stored, err := e.Store.ListInstances()
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, rec := range stored {
		have[rec.ID] = true
	}
	for _, m := range e.Plugins() {
		if m.Singleton && !have[m.ID] {
			rec := store.Instance{ID: m.ID, Plugin: m.ID, Name: m.Name, Enabled: true, Config: map[string]any{}}
			if err := e.Store.SaveInstance(&rec); err != nil {
				return err
			}
			stored = append(stored, rec)
		}
	}
	for _, rec := range stored {
		// Move any secret still stored in plain form into the secret store.
		if sealed, err := e.seal(rec); err == nil && e.Secrets != nil {
			if fmt.Sprint(sealed.Config) != fmt.Sprint(rec.Config) {
				_ = e.Store.SaveInstance(&sealed)
			}
			rec = sealed
		}
		e.launch(rec)
	}
	return nil
}

// Stop stops all instances and waits for in-flight runs.
func (e *Engine) Stop() {
	e.mu.Lock()
	all := e.running
	e.running = map[string]*running{}
	e.mu.Unlock()
	for _, r := range all {
		e.halt(r)
	}
	e.wg.Wait()
}

func (e *Engine) halt(r *running) {
	if r.cancel != nil {
		r.cancel()
	}
	if r.inst != nil {
		if err := r.inst.Stop(); err != nil {
			e.log.Warn("stop instance", "id", r.rec.ID, "err", err)
		}
	}
}

// launch registers the instance and starts it in the background.
func (e *Engine) launch(rec store.Instance) {
	r := &running{rec: rec, status: StatusStopped}
	e.mu.Lock()
	old := e.running[rec.ID]
	e.running[rec.ID] = r
	reg, known := e.plugins[rec.Plugin]
	e.mu.Unlock()
	if old != nil {
		e.halt(old)
	}
	if !rec.Enabled {
		e.setStatus(r, StatusStopped, "")
		return
	}
	if !known {
		e.setStatus(r, StatusError, "插件未安装："+rec.Plugin)
		return
	}
	inst, err := reg.factory(e.unseal(rec).Config)
	if err != nil {
		e.setStatus(r, StatusError, err.Error())
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	r.inst, r.cancel = inst, cancel
	delete(e.trigErrs, rec.ID)
	e.mu.Unlock()
	e.setStatus(r, StatusStarting, "")
	// what the rules ask it to watch is there before it starts
	e.trigMu.Lock()
	e.mu.RLock()
	asked := e.triggersFor(rec.ID)
	e.mu.RUnlock()
	e.giveTriggers(r, inst, asked)
	e.trigMu.Unlock()
	go func() {
		if err := inst.Start(ctx, &host{e: e, rec: rec}); err != nil {
			e.setStatus(r, StatusError, err.Error())
			return
		}
		e.setStatus(r, StatusRunning, "")
	}()
}

func (e *Engine) setStatus(r *running, status, errMsg string) {
	e.mu.Lock()
	current := e.running[r.rec.ID] == r
	r.status, r.err = status, errMsg
	rec := r.rec
	e.mu.Unlock()
	view := InstanceView{Instance: e.masked(rec), Status: status, Error: errMsg}
	if current {
		e.publish(Message{Kind: "instance", Data: view})
	}
}

// ReportStatus lets an instance adapter report a status change after start
// (for example an external plugin process that crashed and is restarting).
func (e *Engine) ReportStatus(instanceID, status, errMsg string) {
	e.mu.RLock()
	r := e.running[instanceID]
	e.mu.RUnlock()
	if r != nil {
		e.setStatus(r, status, errMsg)
	}
}

// ---- instances ----

func (e *Engine) Instances() []InstanceView {
	e.mu.RLock()
	out := make([]InstanceView, 0, len(e.running))
	for _, r := range e.running {
		out = append(out, InstanceView{Instance: r.rec, Status: r.status, Error: r.err})
	}
	e.mu.RUnlock()
	for i := range out {
		out[i].Instance = e.masked(out[i].Instance)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (e *Engine) Instance(instanceID string) (InstanceView, bool) {
	e.mu.RLock()
	r, ok := e.running[instanceID]
	var view InstanceView
	if ok {
		view = InstanceView{Instance: r.rec, Status: r.status, Error: r.err}
	}
	e.mu.RUnlock()
	if !ok {
		return InstanceView{}, false
	}
	view.Instance = e.masked(view.Instance)
	return view, true
}

// SaveInstance creates or updates an instance and (re)starts it.
func (e *Engine) SaveInstance(rec store.Instance) (store.Instance, error) {
	if _, ok := e.Plugin(rec.Plugin); !ok {
		return rec, fmt.Errorf("unknown plugin %q", rec.Plugin)
	}
	if rec.ID == "" {
		rec.ID = rec.Plugin + "-" + id.Token(4)
	}
	if rec.Name == "" {
		rec.Name = rec.ID
	}
	if rec.Config == nil {
		rec.Config = map[string]any{}
	}
	rec, err := e.seal(rec)
	if err != nil {
		return rec, fmt.Errorf("保存密钥失败：%w", err)
	}
	if err := e.Store.SaveInstance(&rec); err != nil {
		return rec, err
	}
	e.launch(rec)
	return e.masked(rec), nil
}

func (e *Engine) DeleteInstance(instanceID string) error {
	e.mu.Lock()
	r := e.running[instanceID]
	delete(e.running, instanceID)
	delete(e.trigErrs, instanceID)
	e.mu.Unlock()
	if r != nil {
		e.halt(r)
		if e.Secrets != nil {
			for _, f := range e.secretFields(r.rec.Plugin) {
				_ = e.Secrets.Delete(instanceID + "/" + f)
			}
		}
	}
	return e.Store.DeleteInstance(instanceID)
}

func (e *Engine) instance(instanceID string) (plugin.Instance, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	r, ok := e.running[instanceID]
	if !ok {
		return nil, fmt.Errorf("实例不存在：%s", instanceID)
	}
	if r.inst == nil || r.status == StatusStopped {
		return nil, fmt.Errorf("实例未运行：%s", instanceID)
	}
	return r.inst, nil
}

// Ingress forwards an HTTP request to an instance.
func (e *Engine) Ingress(ctx context.Context, instanceID string, req plugin.IngressRequest) (plugin.IngressResponse, error) {
	inst, err := e.instance(instanceID)
	if err != nil {
		return plugin.IngressResponse{Status: 404}, nil
	}
	return inst.HandleIngress(ctx, req)
}

// Options asks an instance what may go in one of its fields.
func (e *Engine) Options(ctx context.Context, instanceID, action, field string) ([]plugin.Option, error) {
	inst, err := e.instance(instanceID)
	if err != nil {
		return nil, err
	}
	return inst.Options(ctx, action, field)
}

// Invoke calls an action on an instance directly (used by tests and the UI).
func (e *Engine) Invoke(ctx context.Context, instanceID, action string, params map[string]any, ictx plugin.InvokeContext) (map[string]any, error) {
	inst, err := e.instance(instanceID)
	if err != nil {
		return nil, err
	}
	ictx.InstanceID = instanceID
	return inst.Invoke(ctx, action, params, ictx)
}

// ---- host ----

type host struct {
	e   *Engine
	rec store.Instance
}

func (h *host) Emit(ev *event.Event) (string, error) {
	ev.Source, ev.Plugin = h.rec.ID, h.rec.Plugin
	return h.e.Emit(ev)
}

func (h *host) Log(level, msg string) {
	h.e.log.Log(context.Background(), logLevel(level), "plugin: "+msg, "instance", h.rec.ID, "plugin", h.rec.Plugin)
	h.e.publish(Message{Kind: "log", Data: LogEntry{Instance: h.rec.ID, Level: level, Message: msg, Time: time.Now()}})
}

// logLevel maps a plugin's level name onto the hub's own; anything it does
// not recognise is logged as info.
func logLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func (h *host) StateGet(key string) (string, bool) { return h.e.Store.KVGet("instance:"+h.rec.ID, key) }
func (h *host) StateSet(key, value string) error {
	return h.e.Store.KVSet("instance:"+h.rec.ID, key, value)
}

func (h *host) InstanceID() string { return h.rec.ID }

func (h *host) SetStatus(status, message string) { h.e.ReportStatus(h.rec.ID, status, message) }

func (h *host) TriggerErrors(errs map[string]string) { h.e.triggerErrors(h.rec.ID, errs) }

func (h *host) DataDir() string {
	return h.e.InstanceDir(h.rec.ID)
}

// InstanceDir is the private, persistent directory of an instance.
func (e *Engine) InstanceDir(instanceID string) string {
	dir := filepath.Join(e.dataDir, "instances", instanceID)
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// ---- subscriptions ----

// Subscribe returns a channel of live messages and a function to cancel it.
func (e *Engine) Subscribe() (<-chan Message, func()) {
	ch := make(chan Message, 128)
	e.subMu.Lock()
	e.subs[ch] = struct{}{}
	e.subMu.Unlock()
	return ch, func() {
		e.subMu.Lock()
		delete(e.subs, ch)
		e.subMu.Unlock()
	}
}

func (e *Engine) publish(m Message) {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	for ch := range e.subs {
		select {
		case ch <- m:
		default: // slow subscriber: drop rather than block the hub
		}
	}
}

// Publish pushes a message to live subscribers; used by engine extensions.
func (e *Engine) Publish(kind string, data any) { e.publish(Message{Kind: kind, Data: data}) }
