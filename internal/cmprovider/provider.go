package cmprovider

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/golang/glog"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/metrics/pkg/apis/custom_metrics"

	// TODO: Vendor this
	cmaprovider "github.com/draios/kubernetes-sysdig-metrics-apiserver/internal/custom-metrics-apiserver/pkg/provider"

	"github.com/draios/kubernetes-sysdig-metrics-apiserver/internal/sdc"
)

type sysdigProvider struct {
	mapper               apimeta.RESTMapper
	kubeClient           dynamic.ClientPool
	sysdigClient         *sdc.Client
	sysdigRequestTimeout time.Duration

	MetricsRegistry
}

var Cluster = ""

func NewSysdigProvider(mapper apimeta.RESTMapper, kubeClient dynamic.ClientPool, sysdigClient *sdc.Client, sysdigRequestTimeout time.Duration, updateInterval time.Duration, stopChan <-chan struct{}) cmaprovider.CustomMetricsProvider {
	lister := &cachingMetricsLister{
		sysdigClient:         sysdigClient,
		sysdigRequestTimeout: sysdigRequestTimeout,
		updateInterval:       updateInterval,
		MetricsRegistry:      &registry{},
	}
	lister.RunUntil(stopChan)
	return &sysdigProvider{
		kubeClient:           kubeClient,
		mapper:               mapper,
		sysdigClient:         sysdigClient,
		sysdigRequestTimeout: sysdigRequestTimeout,
		MetricsRegistry:      lister,
	}
}

func (p *sysdigProvider) metricFor(value float64, ts time.Time, groupResource schema.GroupResource, namespace string, serviceName string, workloadType string, metricName string) (*custom_metrics.MetricValue, error) {
	kind, err := p.mapper.KindFor(groupResource.WithVersion(""))
	if err != nil {
		return nil, err
	}
	var (
		quantity = *resource.NewMilliQuantity(int64(value*1000), resource.DecimalSI)
		version  = groupResource.Group + "/" + runtime.APIVersionInternal
	)
	glog.V(10).Infof("Returning value %s for metric %s (version=%s, kind=%s, name=%s, namespace=%s, ts=%s, workloadType=%s)",
		quantity.String(), metricName, version, kind.Kind, serviceName, namespace, ts.String(), workloadType)
	return &custom_metrics.MetricValue{
		DescribedObject: custom_metrics.ObjectReference{
			APIVersion: groupResource.Group + "/" + runtime.APIVersionInternal,
			Kind:       kind.Kind,
			Name:       serviceName,
			Namespace:  namespace,
		},
		MetricName: metricName,
		Timestamp:  metav1.Time{Time: ts},
		Value:      quantity,
	}, nil
}

func (p *sysdigProvider) getSingle(info cmaprovider.CustomMetricInfo, namespace, serviceName string, workloadType string) (*custom_metrics.MetricValue, error) {
	if _, ok := p.Metric(info.Metric); !ok {
		return nil, cmaprovider.NewMetricNotFoundError(info.GroupResource, info.Metric)
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.sysdigRequestTimeout)
	defer cancel()
	req := &sdc.GetDataRequest{Last: 10, Sampling: 10}
	clusterFilter := "kubernetes.cluster.name='" + Cluster + "' and "
	filter := clusterFilter + "kubernetes.namespace.name='" + namespace + "' and kubernetes.workload.name='" + serviceName + "' and kubernetes.workload.type='" + workloadType + "'"
	req = req.
		WithMetric(info.Metric, &sdc.MetricAggregation{Group: "avg", Time: "Avg"}).
		WithFilter(fmt.Sprintf(filter))
	payload, _, err := p.sysdigClient.Data.Get(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("sysdig client error: %v", err)
	}
	if len(payload.Samples) == 0 {
		return p.metricFor(0, time.Now(), info.GroupResource, namespace, serviceName, workloadType, info.Metric)
	}
	val, err := payload.FirstValue()
	if err != nil {
		return nil, cmaprovider.NewMetricNotFoundError(info.GroupResource, info.Metric)
	}
	float, err := strconv.ParseFloat(string(val), 64)
	if err != nil {
		return nil, fmt.Errorf("sysdig client returned a value that cannot be parsed as a float: %v", string(val))
	}
	return p.metricFor(float, time.Time(payload.Samples[0].Time), info.GroupResource, namespace, serviceName, workloadType, info.Metric)
}

// GetRootScopedMetricByName fetches a particular metric for a particular root-scoped object.
func (p *sysdigProvider) GetRootScopedMetricByName(groupResource schema.GroupResource, name string, metricName string) (*custom_metrics.MetricValue, error) {
	glog.V(10).Infof("GetRootScopedMetricByName() - groupResource=%s name=%s metricName=%s", groupResource.String(), name, metricName)
	info := cmaprovider.CustomMetricInfo{
		GroupResource: groupResource,
		Metric:        metricName,
		Namespaced:    false,
		Cluster:       Cluster,
	}
	return p.getSingle(info, "", name, "")
}

// GetRootScopedMetricByName fetches a particular metric for a set of root-scoped objects matching the given label
// selector.
func (p *sysdigProvider) GetRootScopedMetricBySelector(groupResource schema.GroupResource, selector labels.Selector, metricName string) (*custom_metrics.MetricValueList, error) {
	glog.V(10).Infof("GetRootScopedMetricBySelector() - groupResource=%s selector=%s metricName=%s", groupResource.String(), selector.String(), metricName)

	// TODO: not implemented yet!
	return nil, cmaprovider.NewMetricNotFoundError(groupResource, metricName)
}

// GetNamespacedMetricByName fetches a particular metric for a particular namespaced object.
func (p *sysdigProvider) GetNamespacedMetricByName(groupResource schema.GroupResource, namespace string, name string, workloadType string, metricName string) (*custom_metrics.MetricValue, error) {
	glog.V(10).Infof("GetNamespacedMetricByName() - groupResource=%s namespace=%s name=%s metricName=%s", groupResource.String(), namespace, name, metricName)
	info := cmaprovider.CustomMetricInfo{
		GroupResource: groupResource,
		Metric:        metricName,
		Namespaced:    true,
		Cluster:       Cluster,
	}
	return p.getSingle(info, namespace, name, workloadType)
}

// GetNamespacedMetricBySelector fetches a particular metric for a set of namespaced objects matching the given label selector.
func (p *sysdigProvider) GetNamespacedMetricBySelector(groupResource schema.GroupResource, namespace string, selector labels.Selector, metricName string) (*custom_metrics.MetricValueList, error) {
	glog.V(10).Infof("GetNamespacedMetricBySelector() - groupResource=%s namespace=%s selector=%s metricName=%s", groupResource.String(), namespace, selector, metricName)

	// TODO: not implemented yet!
	return nil, cmaprovider.NewMetricNotFoundError(groupResource, metricName)
}

type cachingMetricsLister struct {
	sysdigClient         *sdc.Client
	sysdigRequestTimeout time.Duration
	updateInterval       time.Duration

	MetricsRegistry
}

func (l *cachingMetricsLister) Run() {
	l.RunUntil(wait.NeverStop)
}

func (l *cachingMetricsLister) RunUntil(stopChan <-chan struct{}) {
	go wait.Until(func() {
		if err := l.updateMetrics(); err != nil {
			utilruntime.HandleError(err)
		}
	}, l.updateInterval, stopChan)
}

func (l *cachingMetricsLister) updateMetrics() error {
	ctx, cancel := context.WithTimeout(context.Background(), l.sysdigRequestTimeout)
	defer cancel()
	metrics, _, err := l.sysdigClient.Data.Metrics(ctx)
	if err != nil {
		return fmt.Errorf("unable to fetch list of all available metrics: %v", err)
	}
	l.UpdateMetrics(metrics)
	return nil
}

func SetCluster(cluster string) {
	Cluster = cluster
}
