package config

import "knative.dev/serving/pkg/apis/networking/v1alpha1"

type StatusManager interface {
	IsReady(snapshotID string, Ingresses []*v1alpha1.Ingress) (bool, error)
}
