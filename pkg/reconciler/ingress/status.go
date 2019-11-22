/*
Copyright 2019 The Knative Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ingress

import (
	"context"
	"kourier/pkg/config"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"knative.dev/pkg/system"

	"knative.dev/serving/pkg/apis/networking/v1alpha1"

	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"

	"knative.dev/serving/pkg/network/prober"
)

const (
	probeConcurrency          = 5
	stateExpiration           = 5 * time.Minute
	cleanupPeriod             = 1 * time.Minute
	gatewayLabelSelectorKey   = "app"
	gatewayLabelSelectorValue = "3scale-kourier-gateway"
)

// snapshotState represents the probing progress at the Ingress scope
type snapshotState struct {
	id        string
	ingresses []*v1alpha1.Ingress
	// pendingCount is the number of pods that haven't been successfully probed yet
	pendingCount int32
	lastAccessed time.Time

	cancel func()
}

// podState represents the probing progress at the Pod scope
type podState struct {
	successCount int32

	context context.Context
	cancel  func()
}

type workItem struct {
	snapshotState *snapshotState
	podState      *podState
	url           string
	podIP         string
	hostname      string
}

// StatusProber provides a way to check if an ingress is ready by probing the Envoy pods
// managed by this controller and checking if those have the latest snapshot revision.
type StatusProber struct {
	logger *zap.SugaredLogger

	// mu guards snapshotStates and podStates
	mu             sync.Mutex
	snapshotStates map[string]*snapshotState
	podStates      map[string]*podState

	workQueue workqueue.RateLimitingInterface
	podLister corev1listers.PodLister

	readyCallback    func(snapshotID string, Ingresses []*v1alpha1.Ingress)
	probeConcurrency int
	stateExpiration  time.Duration
	cleanupPeriod    time.Duration
}

// NewStatusProber creates a new instance of StatusProber
func NewStatusProber(
	logger *zap.SugaredLogger,
	podLister corev1listers.PodLister,
	readyCallback func(snapshotID string, Ingresses []*v1alpha1.Ingress)) *StatusProber {
	return &StatusProber{
		logger:         logger,
		snapshotStates: make(map[string]*snapshotState),
		podStates:      make(map[string]*podState),
		workQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.DefaultControllerRateLimiter(),
			"ProbingQueue"),
		readyCallback:    readyCallback,
		podLister:        podLister,
		probeConcurrency: probeConcurrency,
		stateExpiration:  stateExpiration,
		cleanupPeriod:    cleanupPeriod,
	}
}

// IsReady checks if the provided Snapshot is replicated to all the Envoy pods managed by this control plane
// This function is designed to be used by the Ingress controller, i.e. it  will be called in the order of reconciliation.
// This means that if IsReady is called on a config change, with a set of ingresses that will be mark as ready if the
// config has been properly replicated.
func (m *StatusProber) IsReady(snapshotID string, Ingresses []*v1alpha1.Ingress) (bool, error) {

	if ready, ok := func() (bool, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if state, ok := m.snapshotStates[snapshotID]; ok {
			if state.id == snapshotID {
				state.lastAccessed = time.Now()
				return atomic.LoadInt32(&state.pendingCount) == 0, true
			}

			// Cancel the polling for the outdated version
			state.cancel()
			delete(m.snapshotStates, snapshotID)
		}
		return false, false
	}(); ok {
		return ready, nil
	}

	ingCtx, cancel := context.WithCancel(context.Background())
	snapshotState := &snapshotState{
		id:           snapshotID,
		ingresses:    Ingresses,
		pendingCount: 0,
		lastAccessed: time.Now(),
		cancel:       cancel,
	}

	var workItems []*workItem
	selector := labels.NewSelector()
	labelSelector, err := labels.NewRequirement(gatewayLabelSelectorKey, selection.Equals, []string{gatewayLabelSelectorValue})
	if err != nil {
		m.logger.Errorf("Failed to create 'Equals' requirement from %q=%q: %w", gatewayLabelSelectorKey, gatewayLabelSelectorValue, err)
	}
	selector = selector.Add(*labelSelector)

	gatewayPods, err := m.podLister.Pods(system.Namespace()).List(selector)
	if err != nil {
		m.logger.Errorf("failed to get gateway pods: %w", err)
	}

	if len(gatewayPods) == 0 {
		return false, nil
	}

	for _, gwPod := range gatewayPods {
		ip := gwPod.Status.PodIP
		ctx, cancel := context.WithCancel(ingCtx)
		podState := &podState{
			successCount: 0,
			context:      ctx,
			cancel:       cancel,
		}
		// Save the podState to be able to cancel it in case of Pod deletion
		func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.podStates[ip] = podState
		}()

		// Update states and cleanup m.podStates when probing is done or cancelled
		go func(ip string) {
			<-podState.context.Done()
			m.updateStates(snapshotState, podState)

			m.mu.Lock()
			defer m.mu.Unlock()
			// It is critical to check that the current podState is also the one stored in the map
			// before deleting it because it could have been replaced if a new version of the ingress
			// has started being probed.
			if state, ok := m.podStates[ip]; ok && state == podState {
				delete(m.podStates, ip)
			}
		}(ip)

		port := strconv.Itoa(int(config.HttpPortInternal))

		workItem := &workItem{
			snapshotState: snapshotState,
			podState:      podState,
			url:           "http://" + gwPod.Status.PodIP + ":" + port + config.InternalKourierPath,
			podIP:         ip,
			hostname:      config.InternalKourierDomain,
		}
		workItems = append(workItems, workItem)

	}

	snapshotState.pendingCount += int32(len(gatewayPods))

	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.snapshotStates[snapshotID] = snapshotState
	}()
	for _, workItem := range workItems {
		m.workQueue.AddRateLimited(workItem)
		m.logger.Infof("Queuing probe for %s, IP: %s (depth: %d)", workItem.url, workItem.podIP, m.workQueue.Len())
	}

	return len(workItems) == 0, nil
}

// Start starts the StatusManager background operations
func (m *StatusProber) Start(done <-chan struct{}) {
	// Start the worker goroutines
	for i := 0; i < m.probeConcurrency; i++ {
		go func() {
			for m.processWorkItem() {
			}
		}()
	}

	// Cleanup the states periodically
	go wait.Until(m.expireOldStates, m.cleanupPeriod, done)

	// Stop processing the queue when cancelled
	go func() {
		<-done
		m.workQueue.ShutDown()
	}()
}

// CancelPodProbing cancels probing of the provided Pod IP.
func (m *StatusProber) CancelPodProbing(pod *corev1.Pod) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, ok := m.podStates[pod.Status.PodIP]; ok {
		state.cancel()
		delete(m.podStates, pod.Status.PodIP)
	}
}

// expireOldStates removes the states that haven't been accessed in a while.
func (m *StatusProber) expireOldStates() {

	m.mu.Lock()
	defer m.mu.Unlock()
	for key, state := range m.snapshotStates {
		if time.Since(state.lastAccessed) > m.stateExpiration {
			state.cancel()
			delete(m.snapshotStates, key)
		}
	}
}

// processWorkItem processes a single work item from workQueue.
// It returns false when there is no more items to process, true otherwise.
func (m *StatusProber) processWorkItem() bool {
	obj, shutdown := m.workQueue.Get()
	if shutdown {
		return false
	}

	defer m.workQueue.Done(obj)

	// Crash if the item is not of the expected type
	item, ok := obj.(*workItem)
	if !ok {
		m.logger.Fatalf("Unexpected work item type: want: %s, got: %s\n", reflect.TypeOf(&workItem{}).Name(), reflect.TypeOf(obj).Name())
	}
	m.logger.Infof("Processing probe for %s, IP: %s (depth: %d)", item.url, item.podIP, m.workQueue.Len())

	// We disable keepalive to get the latest listener always, if not, we will be hitting the same old envoy listener
	// and the internal path will always return the same snapshot ID.
	transport := &http.Transport{
		DisableKeepAlives: true,
	}

	ok, err := prober.Do(
		item.podState.context,
		transport,
		item.url,
		prober.WithHost(config.InternalKourierDomain),
		prober.ExpectsStatusCodes([]int{http.StatusOK}),
		prober.ExpectsHeader(config.InternalKourierHeader, item.snapshotState.id),
	)

	// In case of cancellation, drop the work item
	select {
	case <-item.podState.context.Done():
		m.workQueue.Forget(obj)
		return true
	default:
	}

	if err != nil || !ok {
		// In case of error, enqueue for retry
		m.workQueue.AddRateLimited(obj)
		m.logger.Errorf("Probing of %s failed, IP: %s, ready: %t, error: %v (depth: %d)", item.url, item.podIP, ok, err, m.workQueue.Len())
	} else {
		m.updateStates(item.snapshotState, item.podState)
	}
	return true
}

func (m *StatusProber) updateStates(snapshotState *snapshotState, podState *podState) {

	if atomic.AddInt32(&podState.successCount, 1) == 1 {
		// This is the first successful probe call for the pod, cancel all other work items for this pod
		podState.cancel()

		// This is the last pod being successfully probed, the Ingress is ready
		if atomic.AddInt32(&snapshotState.pendingCount, -1) == 0 {
			m.readyCallback(snapshotState.id, snapshotState.ingresses)

			// Let's remove the snapshotState as it's been marked ready and we don't use it anymore.
			func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				if _, ok := m.snapshotStates[snapshotState.id]; ok {
					m.snapshotStates[snapshotState.id].cancel()
					delete(m.snapshotStates, snapshotState.id)
				}
				//Cleanup pods.
				for val, podState := range m.podStates {
					podState.cancel()
					delete(m.podStates, val)
				}
			}()

		}
	}
}
