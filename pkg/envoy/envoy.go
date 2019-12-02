package envoy

import (
	"context"
	"fmt"
	"kourier/pkg/config"
	"net"
	"net/http"
	"time"

	"knative.dev/pkg/tracker"

	kubeclient "k8s.io/client-go/kubernetes"

	"knative.dev/pkg/network"

	envoyv2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"
	"github.com/envoyproxy/go-control-plane/pkg/cache"
	xds "github.com/envoyproxy/go-control-plane/pkg/server"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"knative.dev/serving/pkg/apis/networking/v1alpha1"
	"knative.dev/serving/pkg/client/clientset/versioned"
)

const (
	grpcMaxConcurrentStreams = 1000000
	retryInterval            = 1 * time.Second
	retryAmount              = 3
)

type EnvoyXdsServer struct {
	gatewayPort    uint
	managementPort uint
	kubeClient     kubeclient.Interface
	knativeClient  versioned.Interface
	ctx            context.Context
	server         xds.Server
	snapshotCache  cache.SnapshotCache
	statusManager  config.StatusManager
}

// Hasher returns node ID as an ID
type Hasher struct {
}

func (h Hasher) ID(node *core.Node) string {
	if node == nil {
		return "unknown"
	}
	return node.Id
}

func NewEnvoyXdsServer(gatewayPort uint, managementPort uint, kubeClient kubeclient.Interface, knativeClient versioned.Interface, statusManager config.StatusManager) EnvoyXdsServer {
	ctx := context.Background()
	snapshotCache := cache.NewSnapshotCache(true, Hasher{}, nil)
	srv := xds.NewServer(ctx, snapshotCache, nil)

	return EnvoyXdsServer{
		gatewayPort:    gatewayPort,
		managementPort: managementPort,
		kubeClient:     kubeClient,
		knativeClient:  knativeClient,
		ctx:            ctx,
		server:         srv,
		snapshotCache:  snapshotCache,
		statusManager:  statusManager,
	}
}

// RunManagementServer starts an xDS server at the given Port.
func (envoyXdsServer *EnvoyXdsServer) RunManagementServer() {
	port := envoyXdsServer.managementPort
	server := envoyXdsServer.server

	var grpcOptions []grpc.ServerOption
	grpcOptions = append(grpcOptions, grpc.MaxConcurrentStreams(grpcMaxConcurrentStreams))
	grpcServer := grpc.NewServer(grpcOptions...)
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Error("Failed to listen")
	}

	// register services
	discovery.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)
	envoyv2.RegisterEndpointDiscoveryServiceServer(grpcServer, server)
	envoyv2.RegisterClusterDiscoveryServiceServer(grpcServer, server)
	envoyv2.RegisterRouteDiscoveryServiceServer(grpcServer, server)
	envoyv2.RegisterListenerDiscoveryServiceServer(grpcServer, server)

	log.Printf("Starting Management Server on Port %d\n", port)
	go func() {
		if err = grpcServer.Serve(lis); err != nil {
			log.Errorf("%s", err)
		}
	}()
	<-envoyXdsServer.ctx.Done()
	grpcServer.GracefulStop()
}

// RunManagementGateway starts an HTTP gateway to an xDS server.
func (envoyXdsServer *EnvoyXdsServer) RunGateway() {
	port := envoyXdsServer.gatewayPort
	server := envoyXdsServer.server
	ctx := envoyXdsServer.ctx

	log.Printf("Starting HTTP/1.1 gateway on Port %d\n", port)
	httpServer := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: &xds.HTTPGateway{Server: server}}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil {
			panic(err)
		}
	}()

	<-ctx.Done()
	if err := httpServer.Shutdown(ctx); err != nil {
		panic(err)
	}
}

func (envoyXdsServer *EnvoyXdsServer) SetSnapshotForIngresses(nodeId string, Ingresses []*v1alpha1.Ingress, endpointsLister corev1listers.EndpointsLister, tracker tracker.Interface) *Caches {
	localDomainName := network.GetClusterDomainName()

	caches := CachesForIngresses(Ingresses, envoyXdsServer.kubeClient, endpointsLister, localDomainName, tracker)

	envoyXdsServer.SetSnapshotForCaches(&caches, nodeId)
	envoyXdsServer.MarkIngressesReady(Ingresses, caches.snapshotVersion)

	return &caches
}

func (envoyXdsServer *EnvoyXdsServer) SetSnapshotForCaches(caches *Caches, nodeId string) {
	err := envoyXdsServer.snapshotCache.SetSnapshot(nodeId, caches.ToEnvoySnapshot())

	err := envoyXdsServer.snapshotCache.SetSnapshot(nodeId, snapshot)
	if err != nil {
		log.Error(err)
		return
	}
}

func (envoyXdsServer *EnvoyXdsServer) MarkIngressesReady(ingresses []*v1alpha1.Ingress, snapshotVersion string) {

	// We don't need to mark anything as ready, as there are no ingresses.
	if len(Ingresses) == 0 {
		return
	}

	// Start retrying until IsReady gives us the Ok, that means, that our config snapshot has been replicated to all
	// the kourier gateways.
	retries := 0
	for {
		if retries >= retryAmount {
			log.Errorf("Failed to mark snapshot %s as ready after %d retries", snapshotVersion.String(), retries)
			break
		}

		inSync, err := envoyXdsServer.statusManager.IsReady(snapshotVersion.String(), Ingresses)
		if err != nil {
			log.Error(err)
			break
		}

		// If we are inSync, exit the retry cicle
		if inSync {
			return
		}
		time.Sleep(retryInterval)
		retries++
	}
}
