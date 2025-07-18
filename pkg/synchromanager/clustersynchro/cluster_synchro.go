package clustersynchro

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	metricsstore "k8s.io/kube-state-metrics/v2/pkg/metrics_store"

	clusterv1alpha2 "github.com/clusterpedia-io/api/cluster/v1alpha2"
	kubestatemetrics "github.com/clusterpedia-io/clusterpedia/pkg/kube_state_metrics"
	"github.com/clusterpedia-io/clusterpedia/pkg/runtime/discovery"
	"github.com/clusterpedia-io/clusterpedia/pkg/runtime/informer"
	resourceconfigfactory "github.com/clusterpedia-io/clusterpedia/pkg/runtime/resourceconfig/factory"
	"github.com/clusterpedia-io/clusterpedia/pkg/storage"
	"github.com/clusterpedia-io/clusterpedia/pkg/synchromanager/features"
	"github.com/clusterpedia-io/clusterpedia/pkg/synchromanager/resourcesynchro"
	clusterpediafeature "github.com/clusterpedia-io/clusterpedia/pkg/utils/feature"
)

type ClusterSyncConfig struct {
	MetricsStoreBuilder     *kubestatemetrics.MetricsStoreBuilder
	PageSizeForResourceSync int64
}

type ClusterSynchro struct {
	name string

	RESTConfig           *rest.Config
	ClusterStatusUpdater ClusterStatusUpdater

	storage                storage.StorageFactory
	resourceSynchroFactory resourcesynchro.SynchroFactory
	syncConfig             ClusterSyncConfig
	healthChecker          *healthChecker
	dynamicDiscovery       discovery.DynamicDiscoveryInterface
	listerWatcherFactory   informer.DynamicListerWatcherFactory
	eventsListerWatcher    cache.ListerWatcher

	closeOnce sync.Once
	closer    chan struct{}
	closed    chan struct{}

	updateStatusCh chan struct{}
	startRunnerCh  chan struct{}
	stopRunnerCh   chan struct{}

	waitGroup wait.Group

	runnerLock    sync.RWMutex
	handlerStopCh chan struct{}
	// Key is the storage resource.
	// Sometimes the synchronized resource and the storage resource are different
	storageResourceVersions map[schema.GroupVersionResource]storage.ClusterResourceVersions
	storageResourceSynchros sync.Map

	syncResources       atomic.Value // []clusterv1alpha2.ClusterGroupResources
	setSyncResourcesCh  chan struct{}
	resourceNegotiator  *ResourceNegotiator
	groupResourceStatus atomic.Value // *GroupResourceStatus

	runningCondition atomic.Value // metav1.Condition
	healthyCondition atomic.Value // metav1.Condition
}

type ClusterStatusUpdater interface {
	UpdateClusterStatus(ctx context.Context, name string, status *clusterv1alpha2.ClusterStatus) error
}

type RetryableError error

func New(name string, config *rest.Config, storageFactory storage.StorageFactory, updater ClusterStatusUpdater, syncConfig ClusterSyncConfig) (*ClusterSynchro, error) {
	dynamicDiscovery, err := discovery.NewDynamicDiscoveryManager(name, config)
	if err != nil {
		return nil, RetryableError(fmt.Errorf("failed to create dynamic discovery manager: %w", err))
	}

	resourceversions, err := storageFactory.GetResourceVersions(context.TODO(), name)
	if err != nil {
		return nil, RetryableError(fmt.Errorf("failed to get resource versions from storage: %w", err))
	}

	listWatchFactory, err := informer.NewDynamicListerWatcherFactory(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create lister watcher factory: %w", err)
	}

	checkerConfig := *config
	if clusterpediafeature.FeatureGate.Enabled(features.HealthCheckerWithStandaloneTCP) {
		checkerConfig.Dial = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
	}
	healthChecker, err := newHealthChecker(&checkerConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create a cluster health checker: %w", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create cluster client: %w", err)
	}

	synchro := &ClusterSynchro{
		name:                 name,
		RESTConfig:           config,
		ClusterStatusUpdater: updater,
		storage:              storageFactory,

		syncConfig:           syncConfig,
		healthChecker:        healthChecker,
		dynamicDiscovery:     dynamicDiscovery,
		listerWatcherFactory: listWatchFactory,
		eventsListerWatcher: &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return client.CoreV1().Events("").List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return client.CoreV1().Events("").Watch(context.TODO(), options)
			},
		},

		closer: make(chan struct{}),
		closed: make(chan struct{}),

		updateStatusCh: make(chan struct{}, 1),
		startRunnerCh:  make(chan struct{}),
		stopRunnerCh:   make(chan struct{}),

		storageResourceVersions: make(map[schema.GroupVersionResource]storage.ClusterResourceVersions),
	}

	if factory, ok := storageFactory.(resourcesynchro.SynchroFactory); ok {
		synchro.resourceSynchroFactory = factory
	} else {
		synchro.resourceSynchroFactory = DefaultResourceSynchroFactory{}
		registerResourceSynchroMetrics()
	}

	var refresherOnce sync.Once
	synchro.dynamicDiscovery.Prepare(discovery.PrepareConfig{
		ResourceMutationHandler: synchro.resetSyncResources,
		AfterStartFunc: func(_ <-chan struct{}) {
			refresherOnce.Do(func() {
				go synchro.syncResourcesRefresher()
			})
		},
	})

	synchro.resourceNegotiator = &ResourceNegotiator{
		name:                  name,
		resourceConfigFactory: resourceconfigfactory.New(),
		dynamicDiscovery:      synchro.dynamicDiscovery,
	}
	synchro.groupResourceStatus.Store((*GroupResourceStatus)(nil))

	synchro.syncResources.Store([]clusterv1alpha2.ClusterGroupResources(nil))
	synchro.setSyncResourcesCh = make(chan struct{}, 1)

	runningCondition := metav1.Condition{
		Type:               clusterv1alpha2.SynchroRunningCondition,
		Status:             metav1.ConditionFalse,
		Reason:             clusterv1alpha2.SynchroPendingReason,
		Message:            "cluster synchro is created, wait running",
		LastTransitionTime: metav1.Now().Rfc3339Copy(),
	}
	synchro.runningCondition.Store(runningCondition)

	healthyCondition := metav1.Condition{
		Type:               clusterv1alpha2.ClusterHealthyCondition,
		Status:             metav1.ConditionUnknown,
		Reason:             clusterv1alpha2.ClusterMonitorStopReason,
		Message:            "wait cluster synchro's healthy monitor running",
		LastTransitionTime: metav1.Now().Rfc3339Copy(),
	}
	synchro.healthyCondition.Store(healthyCondition)

	synchro.initWithResourceVersions(resourceversions)
	return synchro, nil
}

func (s *ClusterSynchro) GetMetricsWriterList() (writers metricsstore.MetricsWriterList) {
	s.storageResourceSynchros.Range(func(_, value interface{}) bool {
		synchro := value.(resourcesynchro.Synchro)
		if writer := synchro.GetMetricsWriter(); writer != nil {
			writers = append(writers, writer)
		}
		return true
	})
	return
}

func (s *ClusterSynchro) initWithResourceVersions(resourceversions map[schema.GroupVersionResource]storage.ClusterResourceVersions) {
	if len(resourceversions) == 0 {
		return
	}

	for gvr, rvs := range resourceversions {
		if rvs.Resources == nil {
			rvs.Resources = make(map[string]interface{})
		}
		if rvs.Events == nil {
			rvs.Events = make(map[string]interface{})
		}
		s.storageResourceVersions[gvr] = rvs
	}
}

func (s *ClusterSynchro) Run(shutdown <-chan struct{}) {
	runningCondition := metav1.Condition{
		Type:               clusterv1alpha2.SynchroRunningCondition,
		Status:             metav1.ConditionTrue,
		Reason:             clusterv1alpha2.SynchroRunningReason,
		Message:            "cluster synchro is running",
		LastTransitionTime: metav1.Now().Rfc3339Copy(),
	}
	s.runningCondition.Store(runningCondition)

	s.waitGroup.Start(s.monitor)
	s.waitGroup.Start(s.runner)

	go func() {
		defer close(s.closed)

		for range s.updateStatusCh {
			status := s.genClusterStatus()
			if err := s.ClusterStatusUpdater.UpdateClusterStatus(context.TODO(), s.name, status); err != nil {
				klog.ErrorS(err, "Failed to update cluster conditions and sync resources status", "cluster", s.name, "conditions", status.Conditions)
			}
		}
		klog.InfoS("cluster synchro is shutdown", "cluster", s.name)
	}()

	select {
	case <-s.closer:
		<-s.closed
	case <-shutdown:
		s.Shutdown(true)
	}
}

func (s *ClusterSynchro) Shutdown(updateStatus bool) {
	s.closeOnce.Do(func() {
		klog.InfoS("cluster synchro is shutdowning...", "cluster", s.name)
		close(s.closer)

		go func() {
			timer := time.NewTicker(15 * time.Second)
			defer timer.Stop()
			for {
				select {
				case <-timer.C:
				case <-s.closed:
					return
				}

				shutdownCount := 0
				statuses := make(map[string][]string)
				s.storageResourceSynchros.Range(func(key, value interface{}) bool {
					synchro := value.(resourcesynchro.Synchro)
					status := synchro.Status()
					if status.Status == clusterv1alpha2.ResourceSyncStatusStop && status.Reason == "" {
						shutdownCount++
						return true
					}

					gvr := key.(schema.GroupVersionResource)
					sr := fmt.Sprintf("%s,%s,%s", status.Status, status.Reason, synchro.Stage())
					msg := fmt.Sprintf("%s/%s/%s", gvr.Group, gvr.Version, gvr.Resource)
					statuses[sr] = append(statuses[sr], msg)
					return true
				})

				select {
				case <-s.closed:
					return
				default:
					klog.Warningf("Cluster Shutdown Block, cluster=%s, shutdown synchro: %d, block synchro: %+v\n", s.name, shutdownCount, statuses)
				}
			}
		}()

		s.waitGroup.Wait()

		runningCondition := metav1.Condition{
			Type:               clusterv1alpha2.SynchroRunningCondition,
			Status:             metav1.ConditionFalse,
			Reason:             clusterv1alpha2.SynchroShutdownReason,
			Message:            "cluster synchro is shutdown",
			LastTransitionTime: metav1.Now().Rfc3339Copy(),
		}
		s.runningCondition.Store(runningCondition)
		if updateStatus {
			s.updateStatus()
		}
		close(s.updateStatusCh)
	})
	<-s.closed
}

func (s *ClusterSynchro) SetResources(syncResources []clusterv1alpha2.ClusterGroupResources, syncAllCustomResources bool) {
	s.syncResources.Store(syncResources)
	s.resourceNegotiator.SetSyncAllCustomResources(syncAllCustomResources)

	s.resetSyncResources()
}

func (s *ClusterSynchro) resetSyncResources() {
	select {
	case s.setSyncResourcesCh <- struct{}{}:
	default:
	}
}

func (s *ClusterSynchro) syncResourcesRefresher() {
	klog.InfoS("sync resources refresher is running", "cluster", s.name)
	for {
		select {
		case <-s.closer:
			return
		case <-s.setSyncResourcesCh:
		}

		select {
		case <-s.closer:
			return
		default:
		}
		s.refreshSyncResources()
	}
}

func (s *ClusterSynchro) refreshSyncResources() {
	syncResources := s.syncResources.Load().([]clusterv1alpha2.ClusterGroupResources)
	if syncResources == nil {
		return
	}
	groupResourceStatus, storageResourceSyncConfigs := s.resourceNegotiator.NegotiateSyncResources(syncResources)

	lastGroupResourceStatus := s.groupResourceStatus.Load().(*GroupResourceStatus)
	deleted := groupResourceStatus.Merge(lastGroupResourceStatus)

	groupResourceStatus.EnableConcurrent()
	defer groupResourceStatus.DisableConcurrent()
	s.groupResourceStatus.Store(groupResourceStatus)

	// multiple resources may match the same storage resource
	storageGVRToSyncGVRs := groupResourceStatus.GetStorageGVRToSyncGVRs()
	updateSyncConditions := func(storageGVR schema.GroupVersionResource, status, reason, message string) {
		for gvr := range storageGVRToSyncGVRs[storageGVR] {
			groupResourceStatus.UpdateSyncCondition(gvr, status, reason, message)
		}
	}

	func() {
		s.runnerLock.Lock()
		defer s.runnerLock.Unlock()

		for storageGVR, config := range storageResourceSyncConfigs {
			// TODO: if config is changed, don't update resource synchro
			if _, ok := s.storageResourceSynchros.Load(storageGVR); ok {
				continue
			}

			resourceStorage, err := s.storage.NewResourceStorage(config.resourceStorageConfig)
			if err != nil {
				klog.ErrorS(err, "Failed to create resource storage", "cluster", s.name, "storage resource", storageGVR)
				updateSyncConditions(storageGVR, clusterv1alpha2.ResourceSyncStatusPending, "SynchroCreateFailed", fmt.Sprintf("new resource storage failed: %s", err))
				continue
			}

			rvs, ok := s.storageResourceVersions[storageGVR]
			if !ok {
				rvs = storage.ClusterResourceVersions{
					Resources: make(map[string]interface{}),
					Events:    make(map[string]interface{}),
				}
				s.storageResourceVersions[storageGVR] = rvs
			}

			var metricsStore *kubestatemetrics.MetricsStore
			if s.syncConfig.MetricsStoreBuilder != nil {
				metricsStore = s.syncConfig.MetricsStoreBuilder.GetMetricStore(s.name, config.syncResource)
			}
			var eventConfig *resourcesynchro.EventConfig
			if config.syncEvents {
				eventConfig = &resourcesynchro.EventConfig{
					ListerWatcher:    s.eventsListerWatcher,
					ResourceVersions: rvs.Events,
				}
			}
			synchro, err := s.resourceSynchroFactory.NewResourceSynchro(s.name,
				resourcesynchro.Config{
					GroupVersionResource: config.syncResource,
					Kind:                 config.kind,
					ListerWatcher:        s.listerWatcherFactory.ForResource(metav1.NamespaceAll, config.syncResource),
					ObjectConvertor:      config.convertor,
					MetricsStore:         metricsStore,
					ResourceVersions:     rvs.Resources,
					PageSizeForInformer:  s.syncConfig.PageSizeForResourceSync,
					ResourceStorage:      resourceStorage,
					Event:                eventConfig,
				},
			)
			if err != nil {
				klog.ErrorS(err, "Failed to create resource synchro", "cluster", s.name, "storage resource", storageGVR)
				updateSyncConditions(storageGVR, clusterv1alpha2.ResourceSyncStatusPending, "SynchroCreateFailed", fmt.Sprintf("new resource synchro failed: %s", err))
				continue
			}
			s.waitGroup.StartWithChannel(s.closer, synchro.Run)
			s.storageResourceSynchros.Store(storageGVR, synchro)

			// After the synchronizer is successfully created,
			// clean up the reasons and message initialized in the sync condition
			updateSyncConditions(storageGVR, clusterv1alpha2.ResourceSyncStatusUnknown, "", "")

			if s.handlerStopCh != nil {
				select {
				case <-s.handlerStopCh:
				default:
					go synchro.Start(s.handlerStopCh)
				}
			}
		}
	}()

	// close unsynced resource synchros
	removedStorageGVRs := NewGVRSet()
	s.storageResourceSynchros.Range(func(key, _ interface{}) bool {
		storageGVR := key.(schema.GroupVersionResource)
		if _, ok := storageResourceSyncConfigs[storageGVR]; !ok {
			removedStorageGVRs.Insert(storageGVR)
		}
		return true
	})
	for storageGVR := range removedStorageGVRs {
		if synchro, ok := s.storageResourceSynchros.Load(storageGVR); ok {
			select {
			case <-synchro.(resourcesynchro.Synchro).Close():
			case <-s.closer:
				return
			}

			updateSyncConditions(storageGVR, clusterv1alpha2.ResourceSyncStatusStop, "SynchroRemoved", "the resource synchro is moved")
			s.storageResourceSynchros.Delete(storageGVR)
		}
	}

	// clean up unstoraged resources
	for storageGVR := range s.storageResourceVersions {
		if _, ok := storageResourceSyncConfigs[storageGVR]; ok {
			continue
		}

		// Whether the storage resource is cleaned successfully or not, it needs to be deleted from `s.storageResourceVersions`
		delete(s.storageResourceVersions, storageGVR)

		err := s.storage.CleanClusterResource(context.TODO(), s.name, storageGVR)
		if err == nil {
			continue
		}

		// even if err != nil, the resource may have been cleaned up
		klog.ErrorS(err, "Failed to clean cluster resource", "cluster", s.name, "storage resource", storageGVR)
		updateSyncConditions(storageGVR, clusterv1alpha2.ResourceSyncStatusStop, "CleanResourceFailed", err.Error())
		for gvr := range storageGVRToSyncGVRs[storageGVR] {
			// not delete failed gvr
			delete(deleted, gvr)
		}
	}

	for gvr := range deleted {
		groupResourceStatus.DeleteVersion(gvr)
	}
}

func (s *ClusterSynchro) runner() {
	klog.InfoS("cluster synchro runner is running...", "cluster", s.name)
	defer klog.InfoS("cluster synchro runner is stopped", "cluster", s.name)

	for {
		select {
		case <-s.startRunnerCh:
		case <-s.closer:
			return
		}

		select {
		case <-s.stopRunnerCh:
			continue
		case <-s.closer:
			return
		default:
		}

		func() {
			s.runnerLock.Lock()
			defer s.runnerLock.Unlock()
			klog.InfoS("dynamic discovery manager and resource synchros are starting", "cluster", s.name)

			s.handlerStopCh = make(chan struct{})
			go func() {
				select {
				case <-s.closer:
				case <-s.stopRunnerCh:
				}

				close(s.handlerStopCh)
			}()

			go s.dynamicDiscovery.Start(s.handlerStopCh)

			s.storageResourceSynchros.Range(func(_, value interface{}) bool {
				go value.(resourcesynchro.Synchro).Start(s.handlerStopCh)
				return true
			})
		}()

		<-s.handlerStopCh
		klog.InfoS("dynamic discovery manager and resource synchros are stopping", "cluster", s.name)
	}
}

func (s *ClusterSynchro) startRunner() {
	select {
	case <-s.stopRunnerCh:
		s.stopRunnerCh = make(chan struct{})
	default:
	}

	select {
	case <-s.startRunnerCh:
	default:
		close(s.startRunnerCh)
	}
}

func (s *ClusterSynchro) stopRunner() {
	select {
	case <-s.startRunnerCh:
		s.startRunnerCh = make(chan struct{})
	default:
	}

	select {
	case <-s.stopRunnerCh:
	default:
		close(s.stopRunnerCh)
	}
}

func (s *ClusterSynchro) updateStatus() {
	select {
	case s.updateStatusCh <- struct{}{}:
	default:
	}
}

func (s *ClusterSynchro) genClusterStatus() *clusterv1alpha2.ClusterStatus {
	status := &clusterv1alpha2.ClusterStatus{
		Version: s.dynamicDiscovery.ServerVersion().GitVersion,
		Conditions: []metav1.Condition{
			s.runningCondition.Load().(metav1.Condition),
			s.healthyCondition.Load().(metav1.Condition),
		},
	}

	groupResourceStatuses := s.groupResourceStatus.Load().(*GroupResourceStatus)
	if groupResourceStatuses == nil {
		// syn resources have not been set, not update sync resources
		return status
	}

	statuses := groupResourceStatuses.LoadGroupResourcesStatuses()
	for si, status := range statuses {
		for ri, resource := range status.Resources {
			for vi, cond := range resource.SyncConditions {
				gr := schema.GroupResource{Group: status.Group, Resource: resource.Name}
				storageGVR := cond.StorageGVR(gr)
				if value, ok := s.storageResourceSynchros.Load(storageGVR); ok {
					synchro := value.(resourcesynchro.Synchro)
					syncedGVR := synchro.GroupVersionResource()
					if gr != syncedGVR.GroupResource() {
						cond.SyncResource = syncedGVR.GroupResource().String()
					}
					if cond.Version != syncedGVR.Version {
						cond.SyncVersion = syncedGVR.Version
					}

					status := synchro.Status()
					cond.Status = status.Status
					cond.Reason = status.Reason
					cond.Message = status.Message
					cond.InitialListPhase = status.InitialListPhase
					cond.LastTransitionTime = status.LastTransitionTime
				} else {
					if cond.Status == "" {
						cond.Status = clusterv1alpha2.ResourceSyncStatusUnknown
					}
					if cond.Reason == "" {
						cond.Reason = "ResourceSynchroNotFound"
					}
					if cond.Message == "" {
						cond.Message = "not found resource synchro"
					}
					cond.LastTransitionTime = metav1.Now().Rfc3339Copy()
				}
				statuses[si].Resources[ri].SyncConditions[vi] = cond
			}
		}
	}
	status.SyncResources = statuses
	return status
}
