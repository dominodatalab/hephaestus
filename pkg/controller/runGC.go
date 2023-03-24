package controller

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/clientset"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/kubernetes"
	"github.com/go-logr/logr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

type ImageBuildGC struct {
	maxIBRetention int
	hephClient     clientset.Interface
	namespaces     []string
}

func NewImageBuildGC(maxIBRetention int, log logr.Logger, ibNamespaces []string) (
	*ImageBuildGC, error) {
	log.Info("Initializing Kubernetes Hephaestus V1 client")

	config, err := kubernetes.RestConfig()
	if err != nil {
		return nil, err
	}

	hephClient, _ := clientset.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	var ns []string
	if len(ibNamespaces) > 0 {
		log.Info("Limiting GC cleanup", "namespaces", ibNamespaces)
		ns = ibNamespaces
	} else {
		// create k8s client to access all namespaces in the cluster
		k8sClient, err := k8s.NewForConfig(config)
		if err != nil {
			return nil, err
		}
		ns, err = getAllNamespaces(k8sClient)
		if err != nil {
			log.Info("Unable to access cluster namespaces")
			return nil, err
		}
		log.Info("Running GC against all namespaces")
	}

	return &ImageBuildGC{
		maxIBRetention: maxIBRetention,
		hephClient:     hephClient,
		namespaces:     ns,
	}, nil
}

func getAllNamespaces(k8sClient k8s.Interface) ([]string, error) {
	namespaces, err := k8sClient.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var ns []string
	for _, namespace := range namespaces.Items {
		ns = append(ns, namespace.Name)
	}
	return ns, nil
}

func (gc *ImageBuildGC) CleanUpIBs(ctx context.Context, log logr.Logger, namespace string) error {
	crdList, err := gc.hephClient.HephaestusV1().ImageBuilds(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Info("Unable to access a list of IBs, not starting IB clean up", "error", err)
		return err
	}

	listLen := len(crdList.Items)
	if listLen == 0 {
		log.V(1).Info("No build resources found, aborting")
		return nil
	}

	var builds []hephv1.ImageBuild
	for _, ib := range crdList.Items {
		state := ib.Status.Phase
		if state == hephv1.PhaseFailed || state == hephv1.PhaseSucceeded {
			builds = append(builds, ib)
		}
	}

	if len(builds) <= gc.maxIBRetention {
		log.Info("Total resources are less than or equal to retention limit, aborting",
			"resourceCount", len(builds), "retentionCount", gc.maxIBRetention)
		return nil
	}
	log.Info("Total resources eligible for deletion", "count", len(builds))
	sort.Slice(builds, func(i, j int) bool {
		return builds[i].CreationTimestamp.Before(&builds[j].CreationTimestamp)
	})

	var errList []error
	deletePolicy := metav1.DeletePropagationForeground
	for _, build := range builds[:len(builds)-gc.maxIBRetention] {
		if err := gc.hephClient.HephaestusV1().ImageBuilds(namespace).Delete(ctx, build.Name,
			metav1.DeleteOptions{PropagationPolicy: &deletePolicy}); err != nil {
			log.Info("Failed to delete build", "name", build.Name, "namespace", build.Namespace, "error", err)
			errList = append(errList, err)
		}
		log.Info("Deleted build", "name", build.Name, "namespace", build.Namespace)
	}
	if len(errList) > 0 {
		var builder strings.Builder
		for _, err := range errList {
			builder.WriteString(err.Error())
			builder.WriteString("; ")
		}
		return errors.New(builder.String())
	}

	log.Info("Cleanup complete")
	return nil
}

func RunGC(maxIBRetention int, cfg config.Manager) error {
	log := ctrlzap.New(
		ctrlzap.UseDevMode(true),
		ctrlzap.Encoder(zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())),
	)
	log = log.WithName("GC")
	ctx := context.Background()

	gc, err := NewImageBuildGC(maxIBRetention, log, cfg.WatchNamespaces)
	if err != nil {
		log.Info("Error setting up GC", "error", err)
		return err
	}

	log.Info("Launching Image Build Clean up", "maxIBRetention", gc.maxIBRetention, "namespaces", gc.namespaces)
	for _, ns := range gc.namespaces {
		err := gc.CleanUpIBs(ctx, log, ns)
		if err != nil {
			log.Info("Exiting Image Builder GC due to error: ", "error", err)
			os.Exit(1)
		}
	}
	return nil
}
