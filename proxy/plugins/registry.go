package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// StateStore is the persistence the registry needs (implemented by *database.DB).
type StateStore interface {
	ListTransportPluginStates(ctx context.Context) ([]database.TransportPluginState, error)
	SaveTransportPluginState(ctx context.Context, state database.TransportPluginState) error
}

type pluginState struct {
	database.TransportPluginState
	groups map[int64]struct{}
}

type snapshot struct {
	states map[string]*pluginState
}

// Registry holds the compiled-in plugins and the atomically swapped state
// snapshot. The zero value is not usable; use NewRegistry or Default.
type Registry struct {
	mu      sync.RWMutex
	plugins []Plugin
	byID    map[string]Plugin
	// active is the registered plugin list published for lock-free reads on
	// the request path.
	active atomic.Pointer[[]Plugin]
	state  atomic.Pointer[snapshot]

	storeMu sync.Mutex
	store   StateStore

	capture *captureWriter
}

func NewRegistry() *Registry {
	r := &Registry{byID: map[string]Plugin{}}
	r.state.Store(&snapshot{states: map[string]*pluginState{}})
	r.active.Store(&[]Plugin{})
	r.capture = newCaptureWriter()
	return r
}

var defaultRegistry atomic.Pointer[Registry]

func init() { defaultRegistry.Store(NewRegistry()) }

// Default is the process-wide registry core uses.
func Default() *Registry { return defaultRegistry.Load() }

// Register adds a plugin to the default registry. Call it from init().
func Register(p Plugin) { Default().Register(p) }

// Register adds a plugin and declares its override credential key to auth.
// It panics on an invalid or duplicate ID (a programming error).
func (r *Registry) Register(p Plugin) {
	id := p.ID()
	if !database.ValidTransportPluginID(id) {
		panic(fmt.Sprintf("plugins: invalid plugin id %q", id))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byID[id]; exists {
		panic(fmt.Sprintf("plugins: duplicate plugin id %q", id))
	}
	r.byID[id] = p
	r.plugins = append(r.plugins, p)
	list := slices.Clone(r.plugins)
	r.active.Store(&list)
	auth.RegisterTransportPluginOverrideKey(id, OverrideCredentialKey(p))
	if o, ok := p.(StateObserver); ok {
		o.StateChanged(r.State(id))
	}
}

// OverrideCredentialKey returns the plugin's per-account override credential key.
func OverrideCredentialKey(p Plugin) string {
	if key := p.Describe().OverrideCredentialKey; key != "" {
		return key
	}
	return DefaultOverrideCredentialKey(p.ID())
}

// Plugins returns the registered plugins in registration order.
func (r *Registry) Plugins() []Plugin { return *r.active.Load() }

// Get returns a registered plugin.
func (r *Registry) Get(id string) (Plugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byID[id]
	return p, ok
}

// Attach connects the registry to its state store, runs plugin migrations,
// installs the outbox reloader and loads the initial snapshot. The reloader is
// installed before the first load so no outbox event can be missed.
func (r *Registry) Attach(ctx context.Context, db *database.DB) error {
	for _, p := range r.Plugins() {
		if m, ok := p.(Migrator); ok {
			if err := m.Migrate(ctx, db); err != nil {
				return fmt.Errorf("migrate transport plugin %s: %w", p.ID(), err)
			}
		}
	}
	r.storeMu.Lock()
	r.store = db
	r.storeMu.Unlock()
	r.capture.setSink(db)
	auth.SetTransportPluginReloader(r.Reload)
	return r.Reload(ctx)
}

func (r *Registry) stateStore() StateStore {
	r.storeMu.Lock()
	defer r.storeMu.Unlock()
	return r.store
}

// Reload replaces the snapshot with the persisted state.
func (r *Registry) Reload(ctx context.Context) error {
	store := r.stateStore()
	if store == nil {
		return nil
	}
	states, err := store.ListTransportPluginStates(ctx)
	if err != nil {
		return err
	}
	r.applyStates(states)
	return nil
}

func (r *Registry) applyStates(states []database.TransportPluginState) {
	next := &snapshot{states: make(map[string]*pluginState, len(states))}
	for _, state := range states {
		ps := &pluginState{TransportPluginState: state, groups: make(map[int64]struct{}, len(state.GroupIDs))}
		for _, id := range state.GroupIDs {
			ps.groups[id] = struct{}{}
		}
		next.states[state.ID] = ps
	}
	r.state.Store(next)
	for _, p := range r.Plugins() {
		if o, ok := p.(StateObserver); ok {
			o.StateChanged(r.State(p.ID()))
		}
	}
}

// State returns the active state for a plugin (defaults when no row exists).
func (r *Registry) State(id string) database.TransportPluginState {
	if ps := r.state.Load().states[id]; ps != nil {
		state := ps.TransportPluginState
		state.GroupIDs = slices.Clone(state.GroupIDs)
		return state
	}
	return database.TransportPluginState{ID: id, GroupIDs: []int64{}, Config: json.RawMessage(`{}`)}
}

// Save validates and persists a plugin state, then reloads locally. Other
// replicas reload through the outbox event written in the same transaction.
func (r *Registry) Save(ctx context.Context, state database.TransportPluginState) error {
	p, ok := r.Get(state.ID)
	if !ok {
		return fmt.Errorf("unknown transport plugin %q", state.ID)
	}
	if err := database.NormalizeTransportPluginState(&state); err != nil {
		return err
	}
	if v, ok := p.(ConfigValidator); ok {
		if err := v.ValidateConfig(state.Config); err != nil {
			return err
		}
	}
	store := r.stateStore()
	if store == nil {
		return fmt.Errorf("transport plugin store is not attached")
	}
	if err := store.SaveTransportPluginState(ctx, state); err != nil {
		return err
	}
	return r.Reload(ctx)
}

// EnabledFor applies the precedence plugin-forced (AccountForcer) > account
// override > group membership > global switch.
func (r *Registry) EnabledFor(p Plugin, account *auth.Account) bool {
	if account == nil {
		return false
	}
	if f, ok := p.(AccountForcer); ok && f.ForcedFor(account) {
		return true
	}
	id := p.ID()
	if enabled, ok := account.TransportPluginOverride(id); ok {
		return enabled
	}
	ps := r.state.Load().states[id]
	if ps == nil {
		return false
	}
	if len(ps.groups) > 0 && account.InAnyGroup(ps.groups) {
		return true
	}
	return ps.Enabled
}

func supportsKind(p Plugin, kind RequestKind) bool {
	return slices.Contains(p.Describe().Kinds, kind)
}

// BindRequest runs every RequestBinder supporting req.Kind once. Core calls
// it (through AccountFilter) before account selection.
func (r *Registry) BindRequest(req *Request) {
	if req == nil {
		return
	}
	for _, p := range r.Plugins() {
		if b, ok := p.(RequestBinder); ok && supportsKind(p, req.Kind) && req.markBound(p.ID()) {
			b.BindRequest(req)
		}
	}
}

// AccountFilter binds the request, then wraps next with the plugins' account
// rules: a plugin that pins req admits only accounts it is enabled for, and
// every plugin enabled for an account may veto it through Admissible. With no
// registered plugins it returns next unchanged.
func (r *Registry) AccountFilter(ctx context.Context, req *Request, kind RequestKind, model string, next auth.AccountFilter) auth.AccountFilter {
	var list []Plugin
	for _, p := range r.Plugins() {
		if supportsKind(p, kind) {
			list = append(list, p)
		}
	}
	if len(list) == 0 {
		return next
	}
	r.BindRequest(req)
	pinned := make(map[string]bool)
	for _, p := range list {
		if pin, ok := p.(Pinner); ok && req != nil && pin.Pinned(ctx, req) {
			pinned[p.ID()] = true
		}
	}
	return func(account *auth.Account) bool {
		if next != nil && !next(account) {
			return false
		}
		for _, p := range list {
			enabled := r.EnabledFor(p, account)
			if pinned[p.ID()] && !enabled {
				return false
			}
			if !enabled {
				continue
			}
			if ok, _ := p.Admissible(ctx, account, model); !ok {
				return false
			}
		}
		return true
	}
}

// PreferredAccount returns the first soft account preference offered by a
// plugin supporting req.Kind, or 0.
func (r *Registry) PreferredAccount(ctx context.Context, req *Request, model string) int64 {
	if req == nil {
		return 0
	}
	for _, p := range r.Plugins() {
		if pref, ok := p.(AccountPreferrer); ok && supportsKind(p, req.Kind) {
			if id := pref.PreferredAccount(ctx, req, model); id > 0 {
				return id
			}
		}
	}
	return 0
}

// AccountSelected tells every AccountPreferrer supporting req.Kind which
// account scheduling chose.
func (r *Registry) AccountSelected(ctx context.Context, req *Request, account *auth.Account, model string) {
	if req == nil || account == nil {
		return
	}
	for _, p := range r.Plugins() {
		if pref, ok := p.(AccountPreferrer); ok && supportsKind(p, req.Kind) {
			pref.AccountSelected(ctx, req, account, model)
		}
	}
}

// Resolve picks the plugin serving one attempt, or returns nil for native.
// It records the choice on req so usage logging can attribute the attempt.
func (r *Registry) Resolve(ctx context.Context, req *Request, account *auth.Account, model string, kind RequestKind) *Route {
	if req == nil {
		return nil
	}
	index, prior := req.beginAttempt()
	attempt := Attempt{Request: req, Account: account, Model: model, Kind: kind, Index: index, Prior: prior}
	for _, p := range r.Plugins() {
		if !supportsKind(p, kind) || !r.EnabledFor(p, account) {
			continue
		}
		if b, ok := p.(RequestBinder); ok && req.markBound(p.ID()) {
			b.BindRequest(req)
		}
		if p.Select(ctx, attempt) {
			req.setServed(p.ID())
			return &Route{registry: r, plugin: p, attempt: attempt, state: r.State(p.ID())}
		}
	}
	req.setServed(database.TransportNative)
	return nil
}

// RouteFor builds a route for plugin p regardless of enablement and Select.
// Only administrator connection tests use it (an explicit per-test mode).
func (r *Registry) RouteFor(p Plugin, req *Request, account *auth.Account, model string, kind RequestKind) *Route {
	index, prior := req.beginAttempt()
	req.setServed(p.ID())
	return &Route{registry: r, plugin: p, attempt: Attempt{Request: req, Account: account, Model: model, Kind: kind, Index: index, Prior: prior}, state: r.State(p.ID())}
}

// MarkNative records an attempt that core routed natively without asking the
// plugins (e.g. relay accounts), so usage attribution stays per attempt.
func (r *Registry) MarkNative(req *Request) {
	if req == nil {
		return
	}
	req.beginAttempt()
	req.setServed(database.TransportNative)
}

// Route is one attempt served by a plugin.
type Route struct {
	registry *Registry
	plugin   Plugin
	attempt  Attempt
	state    database.TransportPluginState
}

// ID returns the serving plugin's ID.
func (rt *Route) ID() string { return rt.plugin.ID() }

// Plugin returns the serving plugin.
func (rt *Route) Plugin() Plugin { return rt.plugin }

// Execute runs the plugin's upstream call, records sampled captures and wraps
// the response with the plugin's ResponseTransformer.
func (rt *Route) Execute(ctx context.Context, env ReqEnv) (*http.Response, error) {
	env.Request = rt.attempt.Request
	if env.Account == nil {
		env.Account = rt.attempt.Account
	}
	if env.Attempt == 0 {
		env.Attempt = rt.attempt.Index
	}
	env.Config = rt.state.Config
	rec := rt.registry.capture.begin(rt.state, &env)
	rec.request(&env)
	resp, err := rt.plugin.Execute(ctx, &env)
	if err != nil {
		rec.failure(err)
		if h, ok := rt.plugin.(ExecuteErrorHandler); ok {
			h.OnExecuteError(ctx, &env, err)
		}
		return nil, err
	}
	if resp == nil {
		err = fmt.Errorf("transport plugin %s returned no response", rt.plugin.ID())
		rec.failure(err)
		return nil, err
	}
	rec.response(resp)
	if t, ok := rt.plugin.(ResponseTransformer); ok {
		if err := wrapResponse(resp, t, &env); err != nil {
			resp.Body.Close()
			log.Printf("[transport-plugin] %s response transform failed: %v", rt.plugin.ID(), err)
			return nil, err
		}
	}
	return resp, nil
}

// SwapDefault replaces the process-wide registry and returns the previous
// one. It exists for tests that exercise core wiring with test plugins.
func SwapDefault(r *Registry) *Registry {
	return defaultRegistry.Swap(r)
}
