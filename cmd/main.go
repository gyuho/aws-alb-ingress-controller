/*
Copyright 2015 The Kubernetes Authors.

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

package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/metric/collectors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/ticketmaster/aws-sdk-go-cache/cache"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	discovery "k8s.io/apimachinery/pkg/version"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/controller"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/metric"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/version"
)

const (
	// High enough QPS to fit all expected use cases. QPS=0 is not set here, because
	// client code is overriding it.
	defaultQPS = 1e6
	// High enough Burst to fit all expected use cases. Burst=0 is not set here, because
	// client code is overriding it.
	defaultBurst = 1e6

	fakeCertificate = "default-fake-certificate"
)

func main() {
	rand.Seed(time.Now().UnixNano())

	fmt.Println(version.String())

	showVersion, conf, err := parseFlags()
	if showVersion {
		os.Exit(0)
	}

	if err != nil {
		glog.Fatal(err)
	}

	kubeClient, err := createApiserverClient(conf.APIServerHost, conf.KubeConfigFile)
	if err != nil {
		handleFatalInitError(err)
	}

	if conf.Namespace != "" {
		_, err = kubeClient.CoreV1().Namespaces().Get(conf.Namespace, metav1.GetOptions{})
		if err != nil {
			glog.Fatalf("No namespace with name %v found: %v", conf.Namespace, err)
		}
	}

	if conf.ClusterName == "" {
		glog.Fatalf("Cluster name must be defined")
	}

	conf.Client = kubeClient

	cc := cache.NewConfig(5 * time.Minute)

	reg := prometheus.NewRegistry()

	reg.MustRegister(cc.NewCacheCollector(collectors.PrometheusNamespace))
	reg.MustRegister(prometheus.NewGoCollector())
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	mc, err := metric.NewCollector(reg)
	if err != nil {
		glog.Fatalf("Error creating prometheus collectors:  %v", err)
	}
	mc.Start()

	c := controller.NewALBController(conf, mc, cc)
	go handleSigterm(c, func(code int) {
		os.Exit(code)
	})

	mux := http.NewServeMux()

	if conf.EnableProfiling {
		registerProfiler(mux)
	}

	registerHealthz(c, mux)
	registerMetrics(reg, mux)
	registerHandlers(mux)

	go startHTTPServer(conf.HealthzPort, mux)

	c.Start()
}

type exiter func(code int)

func handleSigterm(c *controller.ALBController, exit exiter) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM)
	<-signalChan
	glog.Infof("Received SIGTERM, shutting down")

	exitCode := 0
	if err := c.Stop(); err != nil {
		glog.Infof("Error during shutdown: %v", err)
		exitCode = 1
	}

	glog.Infof("Handled quit, awaiting Pod deletion")
	time.Sleep(10 * time.Second)

	glog.Infof("Exiting with %v", exitCode)
	exit(exitCode)
}

// createApiserverClient creates a new Kubernetes REST client. apiserverHost is
// the URL of the API server in the format protocol://address:port/pathPrefix,
// kubeConfig is the location of a kubeconfig file. If defined, the kubeconfig
// file is loaded first, the URL of the API server read from the file is then
// optionally overridden by the value of apiserverHost.
// If neither apiserverHost nor kubeConfig are passed in, we assume the
// controller runs inside Kubernetes and fallback to the in-cluster config. If
// the in-cluster config is missing or fails, we fallback to the default config.
func createApiserverClient(apiserverHost, kubeConfig string) (*kubernetes.Clientset, error) {
	cfg, err := clientcmd.BuildConfigFromFlags(apiserverHost, kubeConfig)
	if err != nil {
		return nil, err
	}

	cfg.QPS = defaultQPS
	cfg.Burst = defaultBurst
	cfg.ContentType = "application/vnd.kubernetes.protobuf"

	glog.Infof("Creating API client for %s", cfg.Host)

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	var v *discovery.Info

	// The client may fail to connect to the API server in the first request.
	// https://github.com/kubernetes/ingress-nginx/issues/1968
	defaultRetry := wait.Backoff{
		Steps:    10,
		Duration: 1 * time.Second,
		Factor:   1.5,
		Jitter:   0.1,
	}

	var lastErr error
	retries := 0
	glog.V(2).Info("Trying to discover Kubernetes version")
	err = wait.ExponentialBackoff(defaultRetry, func() (bool, error) {
		v, err = client.Discovery().ServerVersion()

		if err == nil {
			return true, nil
		}

		lastErr = err
		glog.V(2).Infof("Unexpected error discovering Kubernetes version (attempt %v): %v", err, retries)
		retries++
		return false, nil
	})

	// err is returned in case of timeout in the exponential backoff (ErrWaitTimeout)
	if err != nil {
		return nil, lastErr
	}

	// this should not happen, warn the user
	if retries > 0 {
		glog.Warningf("Initial connection to the Kubernetes API server was retried %d times.", retries)
	}

	glog.Infof("Running in Kubernetes cluster version v%v.%v (%v) - git (%v) commit %v - platform %v",
		v.Major, v.Minor, v.GitVersion, v.GitTreeState, v.GitCommit, v.Platform)

	return client, nil
}

// Handler for fatal init errors. Prints a verbose error message and exits.
func handleFatalInitError(err error) {
	glog.Fatalf("Error while initiating a connection to the Kubernetes API server. "+
		"This could mean the cluster is misconfigured (e.g. it has invalid API server certificates "+
		"or Service Accounts configuration). Reason: %s\n"+
		"Refer to the troubleshooting guide for more information: "+
		"https://kubernetes.github.io/ingress-nginx/troubleshooting/",
		err)
}

func registerHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/build", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		b, _ := json.Marshal(version.String())
		w.Write(b)
	})

	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		if err != nil {
			glog.Errorf("Unexpected error: %v", err)
		}
	})
}

func registerHealthz(ic *controller.ALBController, mux *http.ServeMux) {
	// expose health check endpoint (/healthz)
	healthz.InstallHandler(mux,
		healthz.PingHealthz,
		ic,
	)
}

func registerMetrics(reg *prometheus.Registry, mux *http.ServeMux) {
	mux.Handle(
		"/metrics",
		promhttp.InstrumentMetricHandler(
			reg,
			promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
		),
	)
}

func registerProfiler(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/heap", pprof.Index)
	mux.HandleFunc("/debug/pprof/mutex", pprof.Index)
	mux.HandleFunc("/debug/pprof/goroutine", pprof.Index)
	mux.HandleFunc("/debug/pprof/threadcreate", pprof.Index)
	mux.HandleFunc("/debug/pprof/block", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}

func startHTTPServer(port int, mux *http.ServeMux) {
	server := &http.Server{
		Addr:              fmt.Sprintf(":%v", port),
		Handler:           mux,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	glog.Fatal(server.ListenAndServe())
}
