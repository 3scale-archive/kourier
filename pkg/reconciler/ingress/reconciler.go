package ingress

import (
	"context"
	"kourier/pkg/envoy"

	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"knative.dev/serving/pkg/apis/networking"
	"knative.dev/serving/pkg/client/listers/networking/v1alpha1"
)

const (
	kourierIngressClassName = "kourier.ingress.networking.knative.dev"
)

type Reconciler struct {
	IngressLister   v1alpha1.IngressLister
	EndpointsLister corev1listers.EndpointsLister
	EnvoyXDSServer  envoy.EnvoyXdsServer
}

func (reconciler *Reconciler) Reconcile(ctx context.Context, key string) error {
	ingresses, err := reconciler.IngressLister.List(labels.SelectorFromSet(map[string]string{
		networking.IngressClassAnnotationKey: kourierIngressClassName,
	}))
	if err != nil {
		return err
	}

	reconciler.EnvoyXDSServer.SetSnapshotForIngresses(
		nodeID,
		ingresses,
		reconciler.EndpointsLister,
	)

	return nil
}
