/*
 *
 * Copyright 2020 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package rls implements the RLS LB policy.
package rls

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal/backoff"
	"google.golang.org/grpc/internal/balancergroup"
	"google.golang.org/grpc/internal/buffer"
	internalgrpclog "google.golang.org/grpc/internal/grpclog"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/resolver"
)

const (
	// Name is the name of the RLS LB policy.
	Name = "rls_experimental"
	// Default frequency for data cache purging.
	periodicCachePurgeFreq = time.Minute
)

var (
	logger = grpclog.Component("rls")

	// Below defined vars for overriding in unit tests.

	// Default exponential backoff strategy for data cache entries.
	defaultBackoffStrategy = backoff.Strategy(backoff.DefaultExponential)
	// Ticker used for periodic data cache purging.
	dataCachePurgeTicker = func() *time.Ticker { return time.NewTicker(periodicCachePurgeFreq) }
	// We want every cache entry to live in the cache for at least this
	// duration. If we encounter a cache entry whose minimum expiration time is
	// in the future, we abort the LRU pass, which may temporarily leave the
	// cache being too large. This is necessary to ensure that in cases where
	// the cache is too small, when we receive an RLS Response, we keep the
	// resulting cache entry around long enough for the pending incoming
	// requests to be re-processed through the new Picker. If we didn't do this,
	// then we'd risk throwing away each RLS response as we receive it, in which
	// case we would fail to actually route any of our incoming requests.
	minEvictDuration = 5 * time.Second

	// Following functions are no-ops in actual code, but can be overridden in
	// tests to give tests visibility into exactly when certain events happen.
	clientConnUpdateHook = func() {}
	dataCachePurgeHook   = func() {}
	resetBackoffHook     = func() {}
)

func init() {
	balancer.Register(&rlsBB{})
}

type rlsBB struct{}

func (rlsBB) Name() string {
	return Name
}

func (rlsBB) Build(cc balancer.ClientConn, opts balancer.BuildOptions) balancer.Balancer {
	lb := &rlsBalancer{
		done:                     grpcsync.NewEvent(),
		cc:                       cc,
		bopts:                    opts,
		purgeTicker:              dataCachePurgeTicker(),
		lbCfg:                    &lbConfig{},
		pendingMap:               make(map[cacheKey]*backoffState),
		childPolicies:            make(map[string]*childPolicyWrapper),
		ccUpdateCh:               make(chan *balancer.ClientConnState, 1),
		childPolicyStateUpdateCh: buffer.NewUnbounded(),
		connectivityStateCh:      make(chan struct{}),
	}
	lb.logger = internalgrpclog.NewPrefixLogger(logger, fmt.Sprintf("[rls-experimental-lb %p] ", lb))
	lb.dataCache = newDataCache(maxCacheSize, lb.logger)
	lb.bg = balancergroup.New(cc, opts, lb, lb.logger)
	lb.bg.Start()
	go lb.run()
	return lb
}

// rlsBalancer implements the RLS LB policy.
type rlsBalancer struct {
	done        *grpcsync.Event
	cc          balancer.ClientConn
	bopts       balancer.BuildOptions
	purgeTicker *time.Ticker
	logger      *internalgrpclog.PrefixLogger

	// If both cacheMu and stateMu need to be acquired, the former must be
	// acquired first to prevent a deadlock. This order restriction is due to the
	// fact that in places where we need to acquire both the locks, we always
	// start off reading the cache.

	// cacheMu guards access to the data cache and pending requests map.
	cacheMu    sync.RWMutex
	dataCache  *dataCache                 // Cache of RLS data.
	pendingMap map[cacheKey]*backoffState // Map of pending RLS requests.

	// stateMu guards access to all LB policy state.
	stateMu            sync.Mutex
	lbCfg              *lbConfig        // Most recently received service config.
	childPolicyBuilder balancer.Builder // Cached child policy builder.
	resolverState      resolver.State   // Cached resolver state.
	ctrlCh             *controlChannel  // Control channel to the RLS server.
	bg                 *balancergroup.BalancerGroup
	childPolicies      map[string]*childPolicyWrapper
	defaultPolicy      *childPolicyWrapper
	// A reference to the most recent picker sent to gRPC as part of a state
	// update is cached in this field so that we can release the reference to the
	// default child policy wrapper when a new picker is created. See
	// sendNewPickerLocked() for details.
	lastPicker *rlsPicker

	// Channels on which updates are received or pushed.
	ccUpdateCh               chan *balancer.ClientConnState
	childPolicyStateUpdateCh *buffer.Unbounded // idAndState from child policy.
	connectivityStateCh      chan struct{}     // signalled when control channel becomes READY again.
}

// run is a long-running goroutine which handles all the updates that the
// balancer wishes to handle. The appropriate updateHandler will push the update
// on to a channel that this goroutine will select on, thereby the handling of
// the update will happen asynchronously.
func (b *rlsBalancer) run() {
	go b.purgeDataCache()
	for {
		select {
		case u := <-b.ccUpdateCh:
			b.handleClientConnUpdate(u)
		case u := <-b.childPolicyStateUpdateCh.Get():
			update := u.(idAndState)
			b.childPolicyStateUpdateCh.Load()
			b.handleChildPolicyStateUpdate(update.id, update.state)
		case <-b.connectivityStateCh:
			b.logger.Infof("Resetting backoff state after control channel getting back to READY")
			b.cacheMu.Lock()
			updatePicker := b.dataCache.resetBackoffState(&backoffState{bs: defaultBackoffStrategy})
			b.cacheMu.Unlock()
			if updatePicker {
				b.sendNewPicker()
			}
			resetBackoffHook()
		case <-b.done.Done():
			return
		}
	}
}

// purgeDataCache is a long-running goroutine which periodically deletes expired
// entries. An expired entry is one for which both the expiryTime and
// backoffExpiryTime are in the past.
func (b *rlsBalancer) purgeDataCache() {
	for {
		select {
		case <-b.done.Done():
			return
		case <-b.purgeTicker.C:
			b.cacheMu.Lock()
			updatePicker := b.dataCache.evictExpiredEntries()
			b.cacheMu.Unlock()
			if updatePicker {
				b.sendNewPicker()
			}
			dataCachePurgeHook()
		}
	}
}

func (b *rlsBalancer) UpdateClientConnState(ccs balancer.ClientConnState) error {
	// Remove unprocessed update from the channel, if one exists, before pushing
	// the most recent one.
	select {
	case <-b.ccUpdateCh:
	default:
	}
	b.ccUpdateCh <- &ccs
	return nil
}

// handleClientConnUpdate handles updates to the service config.
//
// Invoked from the run() goroutine and this will attempt to grab the mutex.
func (b *rlsBalancer) handleClientConnUpdate(ccs *balancer.ClientConnState) {
	if b.done.HasFired() {
		b.logger.Warningf("Received service config after balancer close")
		return
	}

	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	newCfg := ccs.BalancerConfig.(*lbConfig)
	if b.lbCfg.Equal(newCfg) {
		b.logger.Infof("New service config matches existing config")
		return
	}

	// When the RLS server name changes, the old control channel needs to be
	// swapped out for a new one. All state associated with the throttling
	// algorithm is stored on a per-control-channel basis; when we swap out
	// channels, we also swap out the throttling state.
	b.handleControlChannelUpdate(newCfg)

	// If the new config changes the size of the data cache, we might have to
	// evict entries to get the cache size down to the newly specified size.
	if newCfg.cacheSizeBytes != b.lbCfg.cacheSizeBytes {
		b.dataCache.resize(newCfg.cacheSizeBytes)
	}

	// Any changes to child policy name or configuration needs to be handled by
	// either creating new child policies or pushing updates to existing ones.
	b.resolverState = ccs.ResolverState
	b.handleChildPolicyConfigUpdate(newCfg, ccs)

	// Update the copy of the config in the LB policy and send a new picker.
	b.lbCfg = newCfg
	b.sendNewPickerLocked()

	clientConnUpdateHook()
}

// handleControlChannelUpdate handles updates to service config fields which
// influence the control channel to the RLS server.
//
// Caller must hold lb.stateMu.
func (b *rlsBalancer) handleControlChannelUpdate(newCfg *lbConfig) {
	if newCfg.lookupService == b.lbCfg.lookupService && newCfg.lookupServiceTimeout == b.lbCfg.lookupServiceTimeout {
		return
	}

	// Create a new control channel and close the existing one.
	b.logger.Infof("Creating control channel to RLS server at: %v", newCfg.lookupService)
	ctrlCh, err := newControlChannel(newCfg.lookupService, newCfg.lookupServiceTimeout, b.bopts, b.connectivityStateCh)
	if err != nil {
		// This is very uncommon and usually represents a non-transient error.
		// There is not much we can do here other than wait for another update
		// which might fix things.
		b.logger.Errorf("Failed to create control channel to %q: %v", newCfg.lookupService, err)
		return
	}
	if b.ctrlCh != nil {
		b.ctrlCh.close()
	}
	b.ctrlCh = ctrlCh
}

// handleChildPolicyConfigUpdate handles updates to service config fields which
// influence child policy configuration.
//
// Caller must hold lb.stateMu.
func (b *rlsBalancer) handleChildPolicyConfigUpdate(newCfg *lbConfig, ccs *balancer.ClientConnState) {
	// Update child policy builder first since other steps are dependent on this.
	if b.childPolicyBuilder == nil || b.childPolicyBuilder.Name() != newCfg.childPolicyName {
		b.logger.Infof("Child policy changed to %q", newCfg.childPolicyName)
		b.childPolicyBuilder = balancer.Get(newCfg.childPolicyName)
		for _, cpw := range b.childPolicies {
			// If the child policy has changed, we need to remove the old policy
			// from the BalancerGroup and add a new one. The BalancerGroup takes
			// care of closing the old one in this case.
			b.bg.Remove(cpw.target)
			b.bg.Add(cpw.target, b.childPolicyBuilder)
		}
	}

	configSentToDefault := false
	if b.lbCfg.defaultTarget != newCfg.defaultTarget {
		// If the default target has changed, create a new childPolicyWrapper for
		// the new target if required. If a new wrapper is created, add it to the
		// childPolicies map and the BalancerGroup.
		b.logger.Infof("Default target in LB config changing from %q to %q", b.lbCfg.defaultTarget, newCfg.defaultTarget)
		cpw := b.childPolicies[newCfg.defaultTarget]
		if cpw == nil {
			cpw = newChildPolicyWrapper(newCfg.defaultTarget)
			b.childPolicies[newCfg.defaultTarget] = cpw
			b.bg.Add(newCfg.defaultTarget, b.childPolicyBuilder)
			b.logger.Infof("Child policy %q added to BalancerGroup", newCfg.defaultTarget)
		}
		if err := b.buildAndPushChildPolicyConfigs(newCfg.defaultTarget, newCfg, ccs); err != nil {
			cpw.lamify(err)
		}

		// If an old default exists, release its reference. If this was the last
		// reference, remove the child policy from the BalancerGroup and remove the
		// corresponding entry the childPolicies map.
		if b.defaultPolicy != nil {
			if b.defaultPolicy.releaseRef() {
				delete(b.childPolicies, b.lbCfg.defaultTarget)
				b.bg.Remove(b.defaultPolicy.target)
			}
		}
		b.defaultPolicy = cpw
		configSentToDefault = true
	}

	// No change in configuration affecting child policies. Return early.
	if b.lbCfg.childPolicyName == newCfg.childPolicyName && b.lbCfg.childPolicyTargetField == newCfg.childPolicyTargetField && childPolicyConfigEqual(b.lbCfg.childPolicyConfig, newCfg.childPolicyConfig) {
		return
	}

	// If fields affecting child policy configuration have changed, the changes
	// are pushed to the childPolicyWrapper which handles them appropriately.
	for _, cpw := range b.childPolicies {
		if configSentToDefault && cpw.target == newCfg.defaultTarget {
			// Default target has already been taken care of.
			continue
		}
		if err := b.buildAndPushChildPolicyConfigs(cpw.target, newCfg, ccs); err != nil {
			cpw.lamify(err)
		}
	}
}

// buildAndPushChildPolicyConfigs builds the final child policy configuration by
// adding the `targetField` to the base child policy configuration received in
// RLS LB policy configuration. The `targetField` is set to target and
// configuration is pushed to the child policy through the BalancerGroup.
//
// Caller must hold lb.stateMu.
func (b *rlsBalancer) buildAndPushChildPolicyConfigs(target string, newCfg *lbConfig, ccs *balancer.ClientConnState) error {
	jsonTarget, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("failed to marshal child policy target %q: %v", target, err)
	}

	config := newCfg.childPolicyConfig
	targetField := newCfg.childPolicyTargetField
	config[targetField] = jsonTarget
	jsonCfg, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal child policy config %+v: %v", config, err)
	}

	parser, _ := b.childPolicyBuilder.(balancer.ConfigParser)
	parsedCfg, err := parser.ParseConfig(jsonCfg)
	if err != nil {
		return fmt.Errorf("childPolicy config parsing failed: %v", err)
	}

	state := balancer.ClientConnState{ResolverState: ccs.ResolverState, BalancerConfig: parsedCfg}
	b.logger.Infof("Pushing new state to child policy %q: %+v", target, state)
	if err := b.bg.UpdateClientConnState(target, state); err != nil {
		b.logger.Warningf("UpdateClientConnState(%q, %+v) failed : %v", target, ccs, err)
	}
	return nil
}

func (b *rlsBalancer) ResolverError(err error) {
	b.bg.ResolverError(err)
}

func (b *rlsBalancer) UpdateSubConnState(sc balancer.SubConn, state balancer.SubConnState) {
	b.bg.UpdateSubConnState(sc, state)
}

func (b *rlsBalancer) Close() {
	b.done.Fire()

	b.purgeTicker.Stop()
	b.stateMu.Lock()
	if b.ctrlCh != nil {
		b.ctrlCh.close()
	}
	b.bg.Close()
	b.stateMu.Unlock()

	b.cacheMu.Lock()
	b.dataCache.stop()
	b.cacheMu.Unlock()
}

func (b *rlsBalancer) ExitIdle() {
	b.bg.ExitIdle()
}

// sendNewPickerLocked pushes a new picker on to the channel.
//
//
// Note that regardless of what connectivity state is reported, the policy will
// return its own picker, and not a picker that unconditionally queues
// (typically used for IDLE or CONNECTING) or a picker that unconditionally
// fails (typically used for TRANSIENT_FAILURE). This is required because,
// irrespective of the connectivity state, we need to able to perform RLS
// lookups for incoming RPCs and affect the status of queued RPCs based on the
// receipt of RLS responses.
//
// Caller must hold lb.stateMu.
func (b *rlsBalancer) sendNewPickerLocked() {
	aggregatedState := b.aggregatedConnectivityState()

	// Acquire a separate reference for the picker. This is required to ensure
	// that the wrapper held by the old picker is not closed when the default
	// target changes in the config, and a new wrapper is created for the new
	// default target. See handleChildPolicyConfigUpdate() for how config changes
	// affecting the default target are handled.
	if b.defaultPolicy != nil {
		b.defaultPolicy.acquireRef()
	}
	picker := &rlsPicker{
		kbm:           b.lbCfg.kbMap,
		origEndpoint:  b.bopts.Target.Endpoint,
		lb:            b,
		defaultPolicy: b.defaultPolicy,
		ctrlCh:        b.ctrlCh,
		maxAge:        b.lbCfg.maxAge,
		staleAge:      b.lbCfg.staleAge,
		bg:            b.bg,
	}
	picker.logger = internalgrpclog.NewPrefixLogger(logger, fmt.Sprintf("[rls-picker %p] ", picker))
	state := balancer.State{
		ConnectivityState: aggregatedState,
		Picker:            picker,
	}
	b.logger.Infof("New balancer.State: %+v", state)
	b.cc.UpdateState(state)

	if b.lastPicker != nil {
		if b.defaultPolicy != nil {
			b.defaultPolicy.releaseRef()
		}
	}
	b.lastPicker = picker
}

func (b *rlsBalancer) sendNewPicker() {
	b.stateMu.Lock()
	b.sendNewPickerLocked()
	b.stateMu.Unlock()
}

// The aggregated connectivity state reported is determined as follows:
// - If there is at least one child policy in state READY, the connectivity
//   state is READY.
// - Otherwise, if there is at least one child policy in state CONNECTING, the
//   connectivity state is CONNECTING.
// - Otherwise, if there is at least one child policy in state IDLE, the
//   connectivity state is IDLE.
// - Otherwise, all child policies are in TRANSIENT_FAILURE, and the
//   connectivity state is TRANSIENT_FAILURE.
//
// If the RLS policy has no child policies and no configured default target,
// then we will report connectivity state IDLE.
//
// Caller must hold lb.stateMu.
func (b *rlsBalancer) aggregatedConnectivityState() connectivity.State {
	if len(b.childPolicies) == 0 && b.lbCfg.defaultTarget == "" {
		return connectivity.Idle
	}

	var readyN, connectingN, idleN int
	for _, cpw := range b.childPolicies {
		state := (*balancer.State)(atomic.LoadPointer(&cpw.state))
		switch state.ConnectivityState {
		case connectivity.Ready:
			readyN++
		case connectivity.Connecting:
			connectingN++
		case connectivity.Idle:
			idleN++
		}
	}

	switch {
	case readyN > 0:
		return connectivity.Ready
	case connectingN > 0:
		return connectivity.Connecting
	case idleN > 0:
		return connectivity.Idle
	default:
		return connectivity.TransientFailure
	}
}

// idAndState wraps a child policy id and its state update.
type idAndState struct {
	id    string
	state balancer.State
}

// UpdateState is a implementation of the balancergroup.BalancerStateAggregator
// interface. The actual state aggregation functionality is handled
// asynchronously. This method only pushes the state update on to channel read
// and dispatched by the run() goroutine.
func (b *rlsBalancer) UpdateState(id string, state balancer.State) {
	b.childPolicyStateUpdateCh.Put(idAndState{id: id, state: state})
}

// handleChildPolicyStateUpdate provides the state aggregator functionality for
// the BalancerGroup.
//
// This method is invoked by the BalancerGroup whenever a child policy sends a
// state update. We cache the child policy's connectivity state and picker for
// two reasons:
// - to suppress connectivity state transitions from TRANSIENT_FAILURE to states
//   other than READY
// - to delegate picks to child policies
func (b *rlsBalancer) handleChildPolicyStateUpdate(id string, newState balancer.State) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()

	cpw := b.childPolicies[id]
	if cpw == nil {
		// All child policies start with an entry in the map. If ID is not in
		// map, it's either been removed, or never existed.
		b.logger.Warningf("Received state update %+v for missing child policy %q", newState, id)
		return
	}

	oldState := (*balancer.State)(atomic.LoadPointer(&cpw.state))
	if oldState.ConnectivityState == connectivity.TransientFailure && newState.ConnectivityState == connectivity.Connecting {
		// Ignore state transitions from TRANSIENT_FAILURE to CONNECTING, and thus
		// fail pending RPCs instead of queuing them indefinitely when all
		// subChannels are failing, even if the subChannels are bouncing back and
		// forth between CONNECTING and TRANSIENT_FAILURE.
		return
	}
	atomic.StorePointer(&cpw.state, unsafe.Pointer(&newState))
	b.logger.Infof("Child policy %q has new state %+v", id, cpw.state)
	b.sendNewPickerLocked()
}

// acquireChildPolicyReferences attempts to acquire references to
// childPolicyWrappers corresponding to the passed in targets. If there is no
// childPolicyWrapper corresponding to one of the targets, a new one is created
// and added to the BalancerGroup.
func (b *rlsBalancer) acquireChildPolicyReferences(targets []string) []*childPolicyWrapper {
	b.stateMu.Lock()
	var newChildPolicies []*childPolicyWrapper
	for _, target := range targets {
		// If the target exists in the LB policy's childPolicies map. a new
		// reference is taken here and added to the new list.
		if cpw := b.childPolicies[target]; cpw != nil {
			cpw.acquireRef()
			newChildPolicies = append(newChildPolicies, cpw)
			continue
		}

		// If the target does not exist in the child policy map, then a new
		// child policy wrapper is created and added to the new list.
		cpw := newChildPolicyWrapper(target)
		b.childPolicies[target] = cpw
		b.bg.Add(target, b.childPolicyBuilder)
		b.logger.Infof("Child policy %q added to BalancerGroup", target)
		newChildPolicies = append(newChildPolicies, cpw)
		if err := b.buildAndPushChildPolicyConfigs(target, b.lbCfg, &balancer.ClientConnState{
			ResolverState: b.resolverState,
		}); err != nil {
			cpw.lamify(err)
		}
	}
	b.stateMu.Unlock()
	return newChildPolicies
}

// releaseChildPolicyReferences releases references to childPolicyWrappers
// corresponding to the passed in targets. If the release reference was the last
// one, the child policy is removed from the BalancerGroup.
func (b *rlsBalancer) releaseChildPolicyReferences(targets []string) {
	b.stateMu.Lock()
	for _, target := range targets {
		if cpw := b.childPolicies[target]; cpw.releaseRef() {
			delete(b.childPolicies, cpw.target)
			b.bg.Remove(cpw.target)
		}
	}
	b.stateMu.Unlock()
}
