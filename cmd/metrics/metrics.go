package main

import (
	"context"
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	runtimeClassAvailable = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kata_remote_runtimeclass_available",
		Help: "Indicates if the kata-remote RuntimeClass is available (1) or not (0).",
	})

	kataConfigInstallationSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kata_config_installation_success",
		Help: "Indicates if KataConfig installation is successful (1) or not (0).",
	})

	kataRemoteWorkloadSuccessRatio = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kata_remote_workload_success_ratio",
		Help: "Percentage of kata-remote workloads that are running successfully.",
	})

	totalKataRemotePods = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "total_kata_remote_pods",
		Help: "Total number of kata-remote pods across all namespaces, regardless of their status.",
	})
)

func checkKataRemoteWorkloads(clientset *kubernetes.Clientset) {

	pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Printf("Error listing pods: %v", err)
		kataRemoteWorkloadSuccessRatio.Set(0)
		totalKataRemotePods.Set(0)
		return
	}

	totalPods := 0
	successfulPods := 0

	for _, pod := range pods.Items {
		if pod.Spec.RuntimeClassName != nil && *pod.Spec.RuntimeClassName == "kata-remote" {
			totalPods++
			if pod.Status.Phase == "Running" || pod.Status.Phase == "Succeeded" {
				successfulPods++
			}
		}
	}

	if totalPods == 0 {
		kataRemoteWorkloadSuccessRatio.Set(100)
		return
	}

	successRatio := float64(successfulPods) / float64(totalPods) * 100
	kataRemoteWorkloadSuccessRatio.Set(successRatio)
}


func getKubernetesClients() (*kubernetes.Clientset, dynamic.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}

	return clientset, dynamicClient, nil
}

func checkKataRuntimeClass(clientset *kubernetes.Clientset) {
	_, err := clientset.NodeV1().RuntimeClasses().Get(context.TODO(), "kata", metav1.GetOptions{})
	if err == nil {
		runtimeClassAvailable.Set(1)
	} else {
		runtimeClassAvailable.Set(0)
	}
}

func checkKataConfigStatus(dynamicClient dynamic.Interface) {
	kataConfigGVR := schema.GroupVersionResource{
		Group:    "kataconfiguration.openshift.io",
		Version:  "v1",
		Resource: "kataconfigs",
	}

	kataConfigs, err := dynamicClient.Resource(kataConfigGVR).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Printf("Error listing KataConfig: %v", err)
		kataConfigInstallationSuccess.Set(0)
		return
	}

	if len(kataConfigs.Items) == 0 {
		log.Println("No KataConfig resources found.")
		kataConfigInstallationSuccess.Set(0)
		return
	}

	kataConfig := &kataConfigs.Items[0]

	status, found, err := unstructured.NestedMap(kataConfig.Object, "status")
	if err != nil || !found {
		log.Printf("Error reading status from KataConfig: %v", err)
		kataConfigInstallationSuccess.Set(0)
		return
	}

	inProgress, _, _ := unstructured.NestedBool(status, "inProgress")
	readyNodeCount, _, _ := unstructured.NestedInt64(status, "readyNodeCount")
	totalNodeCount, _, _ := unstructured.NestedInt64(status, "totalNodeCount")

	if !inProgress && readyNodeCount == totalNodeCount {
		kataConfigInstallationSuccess.Set(1)
	} else {
		kataConfigInstallationSuccess.Set(0)
	}
}

func main() {
	prometheus.MustRegister(runtimeClassAvailable, kataConfigInstallationSuccess, kataRemoteWorkloadSuccessRatio, totalKataRemotePods)

	clientset, dynamicClient, err := getKubernetesClients()
	if err != nil {
		log.Fatalf("Error setting up Kubernetes clients: %v", err)
	}

	checkKataRuntimeClass(clientset)
	checkKataConfigStatus(dynamicClient)
	checkKataRemoteWorkloads(clientset)

	http.Handle("/metrics", promhttp.Handler())
	log.Println("Starting OSC metrics server on port :8090")
	log.Fatal(http.ListenAndServe(":8090", nil))
}
