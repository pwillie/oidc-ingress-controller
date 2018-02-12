package main

import (
	"flag"

	"github.com/golang/glog"
	"github.com/pwillie/oidc-ingress-controller/pkg/controller"
	"github.com/pwillie/oidc-ingress-controller/pkg/signals"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	source = "oidc-ingress-controller"
)

var (
	masterURL             string
	kubeconfig            string
	clientIDAnnotation    string
	clientAuthNamespace   string
	clientAuthService     string
	clientAuthServicePort int
	versionFlag           bool
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&clientIDAnnotation, "clientannotation", "pwillie/oidc-client", "The Ingress Annotation name that holds the OIDC Client ID")
	flag.StringVar(&clientAuthNamespace, "authns", "my-namespace", "Internal auth service namespace")
	flag.StringVar(&clientAuthService, "authsvc", "my-svc", "Internal auth service service name")
	flag.IntVar(&clientAuthServicePort, "authport", 80, "Internal auth service service port")
	flag.BoolVar(&versionFlag, "version", false, "Version")
}

func main() {
	flag.Parse()

	PrintVersion()
	if versionFlag {
		return
	}

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		glog.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		glog.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	ingressController := controller.NewController(
		kubeClient,
		clientIDAnnotation,
		clientAuthNamespace,
		clientAuthService,
		clientAuthServicePort,
	)

	stopCh := signals.SetupSignalHandler()

	if err = ingressController.Run(2, stopCh); err != nil {
		glog.Fatalf("Error running controller: %s", err.Error())
	}
}
