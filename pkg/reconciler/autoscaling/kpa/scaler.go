/*
Copyright 2019 The Knative Authors

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

package kpa

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"knative.dev/pkg/apis/duck"
	"knative.dev/pkg/injection/clients/dynamicclient"
	"knative.dev/pkg/logging"

	pkgnet "knative.dev/pkg/network"
	"knative.dev/pkg/network/prober"
	"knative.dev/serving/pkg/activator"
	pav1alpha1 "knative.dev/serving/pkg/apis/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/apis/networking"
	nv1a1 "knative.dev/serving/pkg/apis/networking/v1alpha1"
	"knative.dev/serving/pkg/network"
	"knative.dev/serving/pkg/reconciler/autoscaling/config"
	aresources "knative.dev/serving/pkg/reconciler/autoscaling/resources"
	"knative.dev/serving/pkg/resources"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const (
	scaleUnknown = -1
	probePeriod  = 1 * time.Second
	probeTimeout = 45 * time.Second

	// The time after which the PA will be re-enqueued.
	// This number is small, since `handleScaleToZero` below will
	// re-enqueue for the configured grace period.
	reenqeuePeriod = 1 * time.Second

	// TODO(#3456): Remove this buffer once KPA does pod failure diagnostics.
	//
	// KPA will scale the Deployment down to zero if it fails to activate after ProgressDeadlineSeconds,
	// however, after ProgressDeadlineSeconds, the Deployment itself updates its status, which causes
	// the Revision to re-reconcile and diagnose pod failures. If we use the same timeout here, we will
	// race the Revision reconciler and scale down the pods before it can actually surface the pod errors.
	// We should instead do pod failure diagnostics here immediately before scaling down the Deployment.
	activationTimeoutBuffer = 10 * time.Second
)

var probeOptions = []interface{}{
	prober.WithHeader(network.UserAgentKey, network.AutoscalingUserAgent),
	prober.WithHeader(network.ProbeHeaderName, activator.Name),
	prober.ExpectsBody(activator.Name),
	prober.ExpectsStatusCodes([]int{http.StatusOK}),
}

// for mocking in tests
type asyncProber interface {
	Offer(context.Context, string, interface{}, time.Duration, time.Duration, ...interface{}) bool
}

// scaler scales the target of a kpa-class PA up or down including scaling to zero.
type scaler struct {
	psInformerFactory duck.InformerFactory
	dynamicClient     dynamic.Interface
	transport         http.RoundTripper

	// For sync probes.
	activatorProbe func(pa *pav1alpha1.PodAutoscaler, transport http.RoundTripper) (bool, error)

	// For async probes.
	probeManager asyncProber
	enqueueCB    func(interface{}, time.Duration)
}

// newScaler creates a scaler.
func newScaler(ctx context.Context, psInformerFactory duck.InformerFactory, enqueueCB func(interface{}, time.Duration)) *scaler {
	logger := logging.FromContext(ctx)
	transport := pkgnet.NewProberTransport()
	ks := &scaler{
		// Wrap it in a cache, so that we don't stamp out a new
		// informer/lister each time.
		psInformerFactory: psInformerFactory,
		dynamicClient:     dynamicclient.Get(ctx),
		transport:         transport,

		// Production setup uses the default probe implementation.
		activatorProbe: activatorProbe,
		probeManager: prober.New(func(arg interface{}, success bool, err error) {
			logger.Infof("Async prober is done for %v: success?: %v error: %v", arg, success, err)
			// Re-enqueue the PA in any case. If the probe timed out to retry again, if succeeded to scale to 0.
			enqueueCB(arg, reenqeuePeriod)
		}, transport),
		enqueueCB: enqueueCB,
	}
	return ks
}

// Resolves the pa to the probing endpoint Eg. http://hostname:port/healthz
func paToProbeTarget(pa *pav1alpha1.PodAutoscaler) string {
	svc := pkgnet.GetServiceHostname(pa.Status.ServiceName, pa.Namespace)
	port := networking.ServicePort(pa.Spec.ProtocolType)

	return fmt.Sprintf("http://%s:%d/healthz", svc, port)
}

// activatorProbe returns true if via probe it determines that the
// PA is backed by the Activator.
func activatorProbe(pa *pav1alpha1.PodAutoscaler, transport http.RoundTripper) (bool, error) {
	// No service name -- no probe.
	if pa.Status.ServiceName == "" {
		return false, nil
	}
	return prober.Do(context.Background(), transport, paToProbeTarget(pa), probeOptions...)
}

// pre: 0 <= min <= max && 0 <= x
func applyBounds(min, max, x int32) int32 {
	if x < min {
		return min
	}
	if max != 0 && x > max {
		return max
	}
	return x
}

func (ks *scaler) handleScaleToZero(ctx context.Context, pa *pav1alpha1.PodAutoscaler,
	sks *nv1a1.ServerlessService, desiredScale int32) (int32, bool) {
	if desiredScale != 0 {
		return desiredScale, true
	}

	// We should only scale to zero when three of the following conditions are true:
	//   a) enable-scale-to-zero from configmap is true
	//   b) The PA has been active for at least the stable window, after which it
	//			gets marked inactive, and
	//   c) the PA has been backed by the Activator for at least the grace period
	//      of time.
	//  Alternatively, if (a) and the revision did not succeed to activate in
	//  `activationTimeout` time -- also scale it to 0.
	cfgs := config.FromContext(ctx)
	cfgAS := cfgs.Autoscaler

	if !cfgAS.EnableScaleToZero {
		return 1, true
	}
	cfgD := cfgs.Deployment
	activationTimeout := cfgD.ProgressDeadline + activationTimeoutBuffer

	now := time.Now()
	logger := logging.FromContext(ctx)
	switch {
	case pa.Status.IsActivating(): // Active=Unknown
		// If we are stuck activating for longer than our progress deadline, presume we cannot succeed and scale to 0.
		if pa.Status.CanFailActivation(now, activationTimeout) {
			logger.Info("Activation has timed out after ", activationTimeout)
			return desiredScale, true
		}
		ks.enqueueCB(pa, activationTimeout)
		return scaleUnknown, false
	case pa.Status.IsReady(): // Active=True
		// Don't scale-to-zero if the PA is active
		// but return `(0, false)` to mark PA inactive, instead.
		sw := aresources.StableWindow(pa, cfgAS)
		af := pa.Status.ActiveFor(now)
		if af >= sw {
			// If SKS currently is in Serving mode, we do not need to enqueue PA here,
			// since SKS will reconcile to change the mode and when it's done,
			// PA will be reconciled again.
			// Othwerwise, SKS might not meaningfully change and thus
			// PA will not be re-enqueued in time.
			// So enqueue PA for reconcile again in a few seconds.
			if sks.Spec.Mode == nv1a1.SKSOperationModeProxy {
				logger.Debug("SKS is already in proxy mode, auto-re-enqueue PA")
				// Long enough to ensure current iteration is finished.
				ks.enqueueCB(pa, 3*time.Second)
			}
			logger.Info("Can deactivate PA, was active for ", af)
			return desiredScale, false
		}
		// Otherwise, scale down to at most 1 for the remainder of the idle period and then
		// reconcile PA again.
		logger.Infof("Sleeping additionally for %v before can scale to 0", sw-af)
		ks.enqueueCB(pa, sw-af)
		return 1, true
	default: // Active=False
		// Probe synchronously, to see if Activator is already in the path.
		r, err := ks.activatorProbe(pa, ks.transport)
		logger.Infof("Probing activator = %v, err = %v", r, err)
		if r {
			// This enforces that the revision has been backed by the Activator for at least
			// ScaleToZeroGracePeriod time.

			// Most conservative check, if it passes we're good.
			if pa.Status.CanScaleToZero(now, cfgAS.ScaleToZeroGracePeriod) {
				return desiredScale, true
			}

			// Otherwise check how long SKS was in proxy mode.
			// Compute the difference between time we've been proxying with the timeout.
			// If it's positive, that's the time we need to sleep, if negative -- we
			// can scale to zero.
			pf := sks.Status.ProxyFor()
			to := cfgAS.ScaleToZeroGracePeriod - pf
			if to <= 0 {
				logger.Info("Fast path scaling to 0, in proxy mode for: ", pf)
				return desiredScale, true
			}

			// Re-enqueue the PA for reconciliation with timeout of `to` to make sure we wait
			// long enough.
			logger.Info("Enqueueing PA after ", to)
			ks.enqueueCB(pa, to)
			return desiredScale, false
		}

		// Otherwise (any prober failure) start the async probe.
		logger.Info("PA is not yet backed by activator, cannot scale to zero")
		if !ks.probeManager.Offer(context.Background(), paToProbeTarget(pa), pa, probePeriod, probeTimeout, probeOptions...) {
			logger.Info("Probe for revision is already in flight")
		}
		return desiredScale, false
	}
}

func (ks *scaler) applyScale(ctx context.Context, pa *pav1alpha1.PodAutoscaler, desiredScale int32,
	ps *pav1alpha1.PodScalable) error {
	logger := logging.FromContext(ctx)

	gvr, name, err := resources.ScaleResourceArguments(pa.Spec.ScaleTargetRef)
	if err != nil {
		return err
	}

	psNew := ps.DeepCopy()
	psNew.Spec.Replicas = &desiredScale
	patch, err := duck.CreatePatch(ps, psNew)
	if err != nil {
		return err
	}
	patchBytes, err := patch.MarshalJSON()
	if err != nil {
		return err
	}

	_, err = ks.dynamicClient.Resource(*gvr).Namespace(pa.Namespace).Patch(ps.Name, types.JSONPatchType,
		patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to apply scale %d to scale target %s: %w", desiredScale, name, err)
	}

	logger.Debug("Successfully scaled to ", desiredScale)
	return nil
}

// scale attempts to scale the given PA's target reference to the desired scale.
func (ks *scaler) scale(ctx context.Context, pa *pav1alpha1.PodAutoscaler, sks *nv1a1.ServerlessService, desiredScale int32) (int32, error) {
	logger := logging.FromContext(ctx)

	if desiredScale < 0 && !pa.Status.IsActivating() {
		logger.Debug("Metrics are not yet being collected.")
		return desiredScale, nil
	}

	min, max := pa.ScaleBounds()
	if newScale := applyBounds(min, max, desiredScale); newScale != desiredScale {
		logger.Debugf("Adjusting desiredScale to meet the min and max bounds before applying: %d -> %d", desiredScale, newScale)
		desiredScale = newScale
	}

	desiredScale, shouldApplyScale := ks.handleScaleToZero(ctx, pa, sks, desiredScale)
	if !shouldApplyScale {
		return desiredScale, nil
	}

	ps, err := resources.GetScaleResource(pa.Namespace, pa.Spec.ScaleTargetRef, ks.psInformerFactory)
	if err != nil {
		return desiredScale, fmt.Errorf("failed to get scale target %v: %w", pa.Spec.ScaleTargetRef, err)
	}

	currentScale := int32(1)
	if ps.Spec.Replicas != nil {
		currentScale = *ps.Spec.Replicas
	}
	if desiredScale == currentScale {
		return desiredScale, nil
	}

	logger.Infof("Scaling from %d to %d", currentScale, desiredScale)
	return desiredScale, ks.applyScale(ctx, pa, desiredScale, ps)
}
