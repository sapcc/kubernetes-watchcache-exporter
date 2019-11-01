package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/api/core/v1"
	apiv1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	SLEEP_TIME = 5 * time.Minute
)

var (
	metricsAddr    string
	kubeconfigFile string
	kubeContext    string
	labels         = []string{"namespace", "name", "endpoint"}
)

func init() {
	flag.StringVar(&metricsAddr, "listen-address", ":9102", "The address to listen on for HTTP requests.")
	flag.StringVar(&kubeconfigFile, "kubeconfig", "", "Use explicit kubeconfig file")
	flag.StringVar(&kubeContext, "context", "", "Use context")
}

func sigHandler() <-chan struct{} {
	stop := make(chan struct{})
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c,
			syscall.SIGINT,  // Ctrl+C
			syscall.SIGTERM) // Termination Request
		sig := <-c
		glog.Warningf("Signal (%v) Detected, Shutting Down", sig)
		close(stop)
	}()
	return stop
}

func main() {
	flag.Parse()

	kubeconfig, err := kubeConfig(kubeconfigFile, kubeContext)

	if err != nil {
		glog.Fatal("Failed to create kubeconfig", err)
	}

	clientset, err := kubernetes.NewForConfig(kubeconfig)
	if err != nil {
		glog.Fatal("Could not create client set", err)
	}

	kubernetesCounterVecDisparity := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "watchcache_endpoint_disparity_total",
		Help: "Export disparities in endpoints between API watch cache and etcd store.",
	}, labels)
	prometheus.MustRegister(kubernetesCounterVecDisparity)
	if err != nil {
		glog.Fatal("Could not create metric counter", err)
	}

	kubernetesCounterVecMissing := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "watchcache_endpoint_missing_total",
		Help: "Export missing endpoints between API watch cache and etcd store.",
	}, labels)
	prometheus.MustRegister(kubernetesCounterVecMissing)
	if err != nil {
		glog.Fatal("Could not create metric counter", err)
	}

	kubernetesCounterTests := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "watchcache_endpoint_tests_total",
		Help: "Watchcache tests in total",
	})
	prometheus.MustRegister(kubernetesCounterTests)
	if err != nil {
		glog.Fatal("Could not create tests counter", err)
	}

	go func() {
		glog.Info("Starting prometheus metrics.")
		http.Handle("/metrics", promhttp.Handler())
		glog.Warning(http.ListenAndServe(metricsAddr, nil))
	}()

	stopper := sigHandler()
	sharedInformers := informers.NewSharedInformerFactory(clientset, 1*time.Hour)
	endpointsInformer := sharedInformers.Core().V1().Endpoints().Informer()

	glog.Info("Starting informer.")
	go endpointsInformer.Run(stopper)
	sharedInformers.WaitForCacheSync(stopper)

	for {
		kubernetesCounterTests.Inc()

		endpointsDirectList, err := clientset.CoreV1().Endpoints("").List(apiv1.ListOptions{})
		if err != nil {
			glog.Warningf("Could not get list of direct endpoints: %s", err)
			time.Sleep(SLEEP_TIME)
			continue
		}

		objs := endpointsInformer.GetIndexer().List()
		endpointsCachedList := make([]*v1.Endpoints, len(objs))
		for i, v := range objs {
			endpointsCachedList[i] = v.(*v1.Endpoints)
		}

		if len(endpointsDirectList.Items) == 0 || len(endpointsCachedList) == 0 {
			glog.Warning("Got empty endpoint list.")
			time.Sleep(SLEEP_TIME)
			continue
		}

		// Endpoint disparity
		if len(endpointsDirectList.Items) == len(endpointsCachedList) {
			for _, endpointDirect := range endpointsDirectList.Items {
				name := endpointDirect.GetName()
				ns := endpointDirect.GetNamespace()

				obj, ok, err := endpointsInformer.GetIndexer().GetByKey(ns + "/" + name)
				if !ok || err != nil {
					glog.Warningf("Could not get endpoint %s/%s: %s", ns, name, err)
					time.Sleep(SLEEP_TIME)
					continue
				}

				endpointCache := obj.(*v1.Endpoints)
				if endpointDirect.GetResourceVersion() != endpointCache.GetResourceVersion() {
					glog.V(5).Infof("Endpoint diff: %s", cmp.Diff(endpointDirect, endpointCache))
					counter, err := kubernetesCounterVecDisparity.GetMetricWith(
						map[string]string{
							"namespace": ns,
							"name":      name,
							"endpoint":  cmp.Diff(endpointDirect.Subsets, endpointCache.Subsets),
						},
					)
					if err != nil {
						glog.Warningf("Could not get counter: %s", ns, name, err)
					} else {
						counter.Add(1)
					}
				}
			}
		}

		// Endpoint missing in cache
		if len(endpointsDirectList.Items) > len(endpointsCachedList) {
			glog.V(5).Infof("Endpoint count differs: %d != %d", len(endpointsDirectList.Items), len(endpointsCachedList))

			for _, endpointDirect := range endpointsDirectList.Items {
				name := endpointDirect.GetName()
				ns := endpointDirect.GetNamespace()

				_, ok, err := endpointsInformer.GetIndexer().GetByKey(ns + "/" + name)
				if !ok || err != nil {
					glog.Warningf("Endpoint %s/%s missing in cache: %s", ns, name, err)

					counter, err := kubernetesCounterVecMissing.GetMetricWith(
						map[string]string{
							"namespace": ns,
							"name":      name,
							"endpoint":  fmt.Sprintf("%v", endpointDirect.Subsets),
						},
					)
					if err != nil {
						glog.Warningf("Could not get counter: %s", ns, name, err)
					} else {
						counter.Add(1)
					}
				}
			}
		}

		// Endpoint missing in etcd
		if len(endpointsDirectList.Items) < len(endpointsCachedList) {
			glog.V(5).Infof("Endpoint count differs: %d != %d", len(endpointsDirectList.Items), len(endpointsCachedList))

			for _, endpointCached := range endpointsCachedList {
				name := endpointCached.GetName()
				ns := endpointCached.GetNamespace()

				found := false
				for _, endpointDirect := range endpointsDirectList.Items {
					if endpointDirect.Namespace == ns &&
						endpointDirect.Name == name {
						found = true
						break
					}
				}
				if !found {
					glog.Warningf("Endpoint %s/%s missing in etcd: %s", ns, name, err)

					counter, err := kubernetesCounterVecMissing.GetMetricWith(
						map[string]string{
							"namespace": ns,
							"name":      name,
							"endpoint":  fmt.Sprintf("%v", endpointCached.Subsets),
						},
					)
					if err != nil {
						glog.Warningf("Could not get counter: %s", ns, name, err)
					} else {
						counter.Add(1)
					}
				}
			}
		}

		time.Sleep(SLEEP_TIME)
	}
}

func kubeConfig(kubeconfig, context string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}

	if len(context) > 0 {
		overrides.CurrentContext = context
	}

	if len(kubeconfig) > 0 {
		rules.ExplicitPath = kubeconfig
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}
