/*
Copyright 2018 The Knative Authors.

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

package revision

import (
	"context"
	"errors"
	"testing"
	"time"

	// Inject the fakes for informers this controller relies on.
	fakecachingclient "knative.dev/caching/pkg/client/injection/client/fake"
	fakeimageinformer "knative.dev/caching/pkg/client/injection/informers/caching/v1alpha1/image/fake"
	fakekubeclient "knative.dev/pkg/client/injection/kube/client/fake"
	fakedeploymentinformer "knative.dev/pkg/client/injection/kube/informers/apps/v1/deployment/fake"
	_ "knative.dev/pkg/client/injection/kube/informers/core/v1/configmap/fake"
	_ "knative.dev/pkg/client/injection/kube/informers/core/v1/service/fake"
	fakeservingclient "knative.dev/serving/pkg/client/injection/client/fake"
	fakepainformer "knative.dev/serving/pkg/client/injection/informers/autoscaling/v1alpha1/podautoscaler/fake"
	fakerevisioninformer "knative.dev/serving/pkg/client/injection/informers/serving/v1/revision/fake"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-containerregistry/pkg/authn/k8schain"
	"golang.org/x/sync/errgroup"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"

	network "knative.dev/networking/pkg"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/metrics"
	"knative.dev/pkg/reconciler"
	"knative.dev/pkg/system"
	tracingconfig "knative.dev/pkg/tracing/config"
	av1alpha1 "knative.dev/serving/pkg/apis/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/apis/config"
	v1 "knative.dev/serving/pkg/apis/serving/v1"
	autoscalerconfig "knative.dev/serving/pkg/autoscaler/config"
	"knative.dev/serving/pkg/deployment"
	"knative.dev/serving/pkg/reconciler/revision/resources"
	resourcenames "knative.dev/serving/pkg/reconciler/revision/resources/names"

	_ "knative.dev/pkg/metrics/testing"
	. "knative.dev/pkg/reconciler/testing"
)

func newTestControllerWithConfig(t *testing.T, configs []*corev1.ConfigMap, opts ...reconcilerOption) (
	context.Context,
	context.CancelFunc,
	[]controller.Informer,
	*controller.Impl,
	*configmap.ManualWatcher) {

	ctx, cancel, informers := SetupFakeContextWithCancel(t)
	t.Cleanup(cancel) // cancel is reentrant, but permit callers just ignore this, if they don't need it.
	configMapWatcher := &configmap.ManualWatcher{Namespace: system.Namespace()}

	// Prepend so that callers can override.
	opts = append([]reconcilerOption{func(r *Reconciler) {
		r.resolver = &nopResolver{}
	}}, opts...)

	controller := newControllerWithOptions(ctx, configMapWatcher, opts...)

	for _, cm := range append([]*corev1.ConfigMap{{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: system.Namespace(),
			Name:      network.ConfigName,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Namespace: system.Namespace(),
			Name:      logging.ConfigMapName(),
		},
		Data: map[string]string{
			"zap-logger-config":   "{\"level\": \"error\",\n\"outputPaths\": [\"stdout\"],\n\"errorOutputPaths\": [\"stderr\"],\n\"encoding\": \"json\"}",
			"loglevel.queueproxy": "info",
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Namespace: system.Namespace(),
			Name:      config.FeaturesConfigName,
		},
		Data: map[string]string{},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Namespace: system.Namespace(),
			Name:      metrics.ConfigMapName(),
		},
		Data: map[string]string{
			"logging.enable-var-log-collection": "true",
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Namespace: system.Namespace(),
			Name:      tracingconfig.ConfigName,
		},
		Data: map[string]string{
			"enable":          "true",
			"debug":           "true",
			"zipkin-endpoint": "http://zipkin.istio-system.svc.cluster.local:9411/api/v2/spans",
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Namespace: system.Namespace(),
			Name:      autoscalerconfig.ConfigName,
		},
		Data: map[string]string{
			"max-scale-up-rate":                       "11.0",
			"container-concurrency-target-percentage": "0.5",
			"container-concurrency-target-default":    "10.0",
			"stable-window":                           "5m",
			"panic-window":                            "10s",
			"tick-interval":                           "2s",
		},
	}, getTestDeploymentConfigMap(), getTestDefaultsConfigMap()},
		configs...) {
		configMapWatcher.OnChange(cm)
	}

	// The Reconciler won't do any work until it becomes the leader.
	if la, ok := controller.Reconciler.(reconciler.LeaderAware); ok {
		la.Promote(reconciler.UniversalBucket(), func(reconciler.Bucket, types.NamespacedName) {})
	}

	return ctx, cancel, informers, controller, configMapWatcher
}

func createRevision(
	t *testing.T,
	ctx context.Context,
	controller *controller.Impl,
	rev *v1.Revision,
) *v1.Revision {
	t.Helper()
	fakeservingclient.Get(ctx).ServingV1().Revisions(rev.Namespace).Create(rev)
	// Since Reconcile looks in the lister, we need to add it to the informer
	fakerevisioninformer.Get(ctx).Informer().GetIndexer().Add(rev)

	if err := controller.Reconciler.Reconcile(context.Background(), KeyOrDie(rev)); err == nil {
		rev, _, _ = addResourcesToInformers(t, ctx, rev)
	}
	return rev
}

func updateRevision(
	t *testing.T,
	ctx context.Context,
	controller *controller.Impl,
	rev *v1.Revision,
) {
	t.Helper()
	fakeservingclient.Get(ctx).ServingV1().Revisions(rev.Namespace).Update(rev)
	fakerevisioninformer.Get(ctx).Informer().GetIndexer().Update(rev)

	if err := controller.Reconciler.Reconcile(context.Background(), KeyOrDie(rev)); err == nil {
		addResourcesToInformers(t, ctx, rev)
	}
}

func addResourcesToInformers(t *testing.T, ctx context.Context, rev *v1.Revision) (*v1.Revision, *appsv1.Deployment, *av1alpha1.PodAutoscaler) {
	t.Helper()

	rev, err := fakeservingclient.Get(ctx).ServingV1().Revisions(rev.Namespace).Get(rev.Name, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Revisions.Get(%v) = %v", rev.Name, err)
	}
	fakerevisioninformer.Get(ctx).Informer().GetIndexer().Add(rev)

	ns := rev.Namespace

	paName := resourcenames.PA(rev)
	pa, err := fakeservingclient.Get(ctx).AutoscalingV1alpha1().PodAutoscalers(rev.Namespace).Get(paName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("PodAutoscalers.Get(%v) = %v", paName, err)
	} else {
		fakepainformer.Get(ctx).Informer().GetIndexer().Add(pa)
	}

	for _, v := range rev.Spec.Containers {
		imageName := kmeta.ChildName(resourcenames.ImageCache(rev), "-"+v.Name)
		image, err := fakecachingclient.Get(ctx).CachingV1alpha1().Images(rev.Namespace).Get(imageName, metav1.GetOptions{})
		if err != nil {
			t.Errorf("Caching.Images.Get(%v) = %v", imageName, err)
		} else {
			fakeimageinformer.Get(ctx).Informer().GetIndexer().Add(image)
		}
	}

	deploymentName := resourcenames.Deployment(rev)
	deployment, err := fakekubeclient.Get(ctx).AppsV1().Deployments(ns).Get(deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Deployments.Get(%v) = %v", deploymentName, err)
	} else {
		fakedeploymentinformer.Get(ctx).Informer().GetIndexer().Add(deployment)
	}

	return rev, deployment, pa
}

type errorResolver struct {
	err error
}

func (r *errorResolver) Resolve(_ context.Context, _ string, _ k8schain.Options, _ sets.String) (string, error) {
	return "", r.err
}

func TestResolutionFailed(t *testing.T) {
	// Unconditionally return this error during resolution.
	innerError := errors.New("i am the expected error message, hear me ROAR!")
	ctx, _, _, controller, _ := newTestController(t, func(r *Reconciler) {
		r.resolver = &errorResolver{innerError}
	})

	rev := testRevision(testPodSpec())

	createRevision(t, ctx, controller, rev)

	rev, err := fakeservingclient.Get(ctx).ServingV1().Revisions(testNamespace).Get(rev.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal("Couldn't get revision:", err)
	}

	// Ensure that the Revision status is updated.
	for _, ct := range []apis.ConditionType{"ContainerHealthy", "Ready"} {
		got := rev.Status.GetCondition(ct)
		want := &apis.Condition{
			Type:   ct,
			Status: corev1.ConditionFalse,
			Reason: "ContainerMissing",
			Message: v1.RevisionContainerMissingMessage(
				rev.Spec.GetContainer().Image, "failed to resolve image to digest: "+innerError.Error()),
			LastTransitionTime: got.LastTransitionTime,
			Severity:           apis.ConditionSeverityError,
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Unexpected revision conditions diff (-want +got): %v", diff)
		}
	}
}

func TestUpdateRevWithWithUpdatedLoggingURL(t *testing.T) {
	ctx, _, _, controller, watcher := newTestControllerWithConfig(t, []*corev1.ConfigMap{{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: system.Namespace(),
			Name:      metrics.ConfigMapName(),
		},
		Data: map[string]string{
			"logging.enable-var-log-collection": "true",
			"logging.revision-url-template":     "http://old-logging.test.com?filter=${REVISION_UID}",
		},
	}, getTestDeploymentConfigMap(),
	})
	revClient := fakeservingclient.Get(ctx).ServingV1().Revisions(testNamespace)

	rev := testRevision(testPodSpec())
	createRevision(t, ctx, controller, rev)

	// Update controllers logging URL
	watcher.OnChange(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: system.Namespace(),
			Name:      metrics.ConfigMapName(),
		},
		Data: map[string]string{
			"logging.enable-var-log-collection": "true",
			"logging.revision-url-template":     "http://new-logging.test.com?filter=${REVISION_UID}",
		},
	})
	updateRevision(t, ctx, controller, rev)

	updatedRev, err := revClient.Get(rev.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal("Couldn't get revision:", err)
	}

	expectedLoggingURL := "http://new-logging.test.com?filter=" + string(rev.UID)
	if updatedRev.Status.LogURL != expectedLoggingURL {
		t.Errorf("Updated revision does not have an updated logging URL: expected: %s, got: %s", expectedLoggingURL, updatedRev.Status.LogURL)
	}
}

func TestRevWithImageDigests(t *testing.T) {
	ctx, _, _, controller, _ := newTestControllerWithConfig(t, nil)
	rev := testRevision(corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "first",
			Image: "gcr.io/repo/image",
			Ports: []corev1.ContainerPort{{
				ContainerPort: 8888,
			}},
		}, {
			Name:  "second",
			Image: "docker.io/repo/image",
		}, {
			Name:  "third",
			Image: "docker.io/anotherrepo/image",
		}},
	})
	createRevision(t, ctx, controller, rev)
	revClient := fakeservingclient.Get(ctx).ServingV1().Revisions(testNamespace)
	rev, err := revClient.Get(rev.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal("Couldn't get revision:", err)
	}
	if len(rev.Status.ContainerStatuses) < 2 {
		t.Error("Revision status does not have imageDigests")
	}

	rev.Status.DeprecatedImageDigest = "gcr.io/repo/image"
	updateRevision(t, ctx, controller, rev)
	if len(rev.Spec.Containers) != len(rev.Status.ContainerStatuses) {
		t.Error("Image digests does not match with the provided containers")
	}
	for i, c := range rev.Spec.Containers {
		if c.Name != rev.Status.ContainerStatuses[i].Name {
			t.Error("Container statuses do not match the order of containers in spec")
		}
	}
	rev.Status.ContainerStatuses = []v1.ContainerStatuses{}
	updateRevision(t, ctx, controller, rev)
	if len(rev.Status.ContainerStatuses) != 0 {
		t.Error("Failed to update revision")
	}
}

func TestNoQueueSidecarImageUpdateFail(t *testing.T) {
	ctx, _, _, controller, watcher := newTestController(t)

	rev := testRevision(testPodSpec())
	// Update controller config with no side car image
	watcher.OnChange(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "config-controller",
			Namespace: system.Namespace(),
		},
		Data: map[string]string{},
	})
	createRevision(t, ctx, controller, rev)

	// Look for the revision deployment.
	_, err := fakekubeclient.Get(ctx).AppsV1().Deployments(system.Namespace()).Get(rev.Name, metav1.GetOptions{})
	if !apierrs.IsNotFound(err) {
		t.Errorf("Expected revision deployment %s to not exist.", rev.Name)
	}
}

func TestGlobalResyncOnDefaultCMChange(t *testing.T) {
	ctx, cancel, informers, ctrl, watcher := newTestControllerWithConfig(t, nil)

	grp := errgroup.Group{}

	rev := testRevision(testPodSpec())
	revClient := fakeservingclient.Get(ctx).ServingV1().Revisions(rev.Namespace)

	waitInformers, err := controller.RunInformers(ctx.Done(), informers...)
	if err != nil {
		t.Fatal("Failed to start informers:", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := grp.Wait(); err != nil {
			t.Errorf("Wait() = %v", err)
		}
		waitInformers()
	})

	if err := watcher.Start(ctx.Done()); err != nil {
		t.Fatal("Failed to start watcher:", err)
	}

	grp.Go(func() error { return ctrl.Run(1, ctx.Done()) })

	revClient.Create(rev)
	revL := fakerevisioninformer.Get(ctx).Lister()
	if err := wait.PollImmediate(10*time.Millisecond, 5*time.Second, func() (bool, error) {
		// The only error we're getting in the test reasonably is NotFound.
		r, _ := revL.Revisions(rev.Namespace).Get(rev.Name)
		return r != nil && r.Status.ObservedGeneration == r.Generation, nil
	}); err != nil {
		t.Fatal("Failed to see Revision reconciliation:", err)
	}
	t.Log("Saw revision reconciliation")

	// Re-get it and nillify the CC, to ensure defaulting
	// happens as expected.
	rev, _ = revL.Revisions(rev.Namespace).Get(rev.Name)
	rev.Spec.ContainerConcurrency = nil
	rev.Generation++
	revClient.Update(rev)

	watcher.OnChange(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: system.Namespace(),
			Name:      config.DefaultsConfigName,
		},
		Data: map[string]string{
			"container-concurrency": "41",
		},
	})

	paL := fakepainformer.Get(ctx).Lister().PodAutoscalers(rev.Namespace)
	if ierr := wait.PollImmediate(50*time.Millisecond, 3*time.Second, func() (bool, error) {
		pa, err := paL.Get(rev.Name)
		return pa != nil && pa.Spec.ContainerConcurrency == 41, err
	}); ierr != nil {
		t.Fatal("Failed to see PA.Spec CC updated with new CM value:", ierr)
	}
}

func TestGlobalResyncOnConfigMapUpdateRevision(t *testing.T) {
	ctx, cancel, informers, ctrl, watcher := newTestControllerWithConfig(t, nil)

	grp := errgroup.Group{}

	rev := testRevision(testPodSpec())
	revClient := fakeservingclient.Get(ctx).ServingV1().Revisions(rev.Namespace)

	waitInformers, err := controller.RunInformers(ctx.Done(), informers...)
	if err != nil {
		t.Fatal("Failed to start informers:", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := grp.Wait(); err != nil {
			t.Errorf("Wait() = %v", err)
		}
		waitInformers()
	})

	if err := watcher.Start(ctx.Done()); err != nil {
		t.Fatal("Failed to start watcher:", err)
	}

	grp.Go(func() error { return ctrl.Run(1, ctx.Done()) })

	revClient.Create(rev)
	revL := fakerevisioninformer.Get(ctx).Lister()
	if err := wait.PollImmediate(10*time.Millisecond, 5*time.Second, func() (bool, error) {
		// The only error we're getting in the test reasonably is NotFound.
		r, _ := revL.Revisions(rev.Namespace).Get(rev.Name)
		// We only create a single revision, but make sure it is reconciled.
		return r != nil && r.Status.ObservedGeneration == r.Generation, nil
	}); err != nil {
		t.Fatal("Failed to see Revision propagation:", err)
	}
	t.Log("Seen revision propagation")

	watcher.OnChange(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: system.Namespace(),
			Name:      metrics.ConfigMapName(),
		},
		Data: map[string]string{
			"logging.enable-var-log-collection": "true",
			"logging.revision-url-template":     "http://new-logging.test.com?filter=${REVISION_UID}",
		},
	})

	want := "http://new-logging.test.com?filter=" + string(rev.UID)
	if ierr := wait.PollImmediate(50*time.Millisecond, 5*time.Second, func() (bool, error) {
		r, err := revL.Revisions(rev.Namespace).Get(rev.Name)
		return r != nil && r.Status.LogURL == want, err
	}); ierr != nil {
		t.Fatal("Failed to see Revision propagation:", ierr)
	}
}

func TestGlobalResyncOnConfigMapUpdateDeployment(t *testing.T) {
	// Test that changes to the ConfigMap result in the desired changes on an existing
	// deployment.
	configMapToUpdate := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: system.Namespace(),
			Name:      deployment.ConfigName,
		},
		Data: map[string]string{
			"queueSidecarImage": "myAwesomeQueueImage",
		},
	}
	callback := func(t *testing.T) func(runtime.Object) HookResult {
		return func(obj runtime.Object) HookResult {
			deployment := obj.(*appsv1.Deployment)
			t.Logf("Deployment updated: %v", deployment.Name)

			const expected = "myAwesomeQueueImage"

			var got string
			for _, c := range deployment.Spec.Template.Spec.Containers {
				if c.Name == resources.QueueContainerName {
					got = c.Image
					if got == expected {
						return HookComplete
					}
				}
			}

			t.Logf("No update occurred; expected: %s got: %s", expected, got)
			return HookIncomplete
		}
	}

	ctx, cancel, informers, ctrl, watcher := newTestControllerWithConfig(t, nil)

	grp := errgroup.Group{}

	kubeClient := fakekubeclient.Get(ctx)

	rev := testRevision(testPodSpec())
	revClient := fakeservingclient.Get(ctx).ServingV1().Revisions(rev.Namespace)
	h := NewHooks()
	h.OnUpdate(&kubeClient.Fake, "deployments", callback(t))

	// Wait for the deployment creation to trigger the global resync. This
	// avoids the create and update being coalesced into one event.
	h.OnCreate(&kubeClient.Fake, "deployments", func(obj runtime.Object) HookResult {
		watcher.OnChange(configMapToUpdate)
		return HookComplete
	})

	waitInformers, err := controller.RunInformers(ctx.Done(), informers...)
	if err != nil {
		t.Fatal("Failed to start informers:", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := grp.Wait(); err != nil {
			t.Errorf("Wait() = %v", err)
		}
		waitInformers()
	})

	if err := watcher.Start(ctx.Done()); err != nil {
		t.Fatal("Failed to start configuration manager:", err)
	}

	grp.Go(func() error { return ctrl.Run(1, ctx.Done()) })

	revClient.Create(rev)

	if err := h.WaitForHooks(3 * time.Second); err != nil {
		t.Error("Global Resync Failed:", err)
	}
}
