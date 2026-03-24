package component

import (
	"context"
	"errors"
	"sort"
	"time"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	ErrMissingNamespaces = errors.New("no namespaces specified")
	ErrInvalidNamespace  = errors.New("invalid namespace name")
)

const (
	defaultHistoryLimit = 10
	defaultInterval     = 1
)

type ImageBuildGC struct {
	HistoryLimit int
	Interval     int
	Client       client.Client
	Namespaces   []string
}

func (gc *ImageBuildGC) Start(ctx context.Context) error {
	if len(gc.Namespaces) == 0 {
		return ErrMissingNamespaces
	}

	if gc.Interval <= 0 {
		gc.Interval = defaultInterval
	}
	if gc.HistoryLimit <= 0 {
		gc.HistoryLimit = defaultHistoryLimit
	}

	interval := time.Duration(gc.Interval) * time.Hour

	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithName("controller").WithName("imagebuild").WithName("gc"))

	if err := gc.GC(ctx); err != nil {
		log.FromContext(ctx).Error(err, "GC cycle failed")
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := gc.GC(ctx); err != nil {
				log.FromContext(ctx).Error(err, "GC cycle failed")
			}
		}
	}
}

func (gc *ImageBuildGC) GC(ctx context.Context) error {
	namespaces := gc.Namespaces
	if len(namespaces) == 1 && namespaces[0] == "" {
		nsList := &corev1.NamespaceList{}
		if err := gc.Client.List(ctx, nsList); err != nil {
			return err
		}
		namespaces = make([]string, 0, len(nsList.Items))
		for _, ns := range nsList.Items {
			namespaces = append(namespaces, ns.Name)
		}
	}

	var errs []error
	for _, ns := range namespaces {
		errs = append(errs, gc.gc(ctx, ns))
	}
	return errors.Join(errs...)
}

func (gc *ImageBuildGC) gc(ctx context.Context, namespace string) error {
	logger := log.FromContext(ctx)
	if namespace == "" {
		logger.Error(ErrInvalidNamespace, "Namespace cannot be empty")
		return ErrInvalidNamespace
	}

	logger = logger.WithValues("namespace", namespace)
	logger.Info("Image Build GC starting")
	defer logger.Info("Image Build GC finished")

	imageBuilds := &hephv1.ImageBuildList{}
	if err := gc.Client.List(ctx, imageBuilds, client.InNamespace(namespace)); err != nil {
		logger.Error(err, "ImageBuilds.List failed")
		return err
	}

	if len(imageBuilds.Items) == 0 {
		logger.Info("No ImageBuilds found")
		return nil
	}

	var builds []hephv1.ImageBuild
	for _, ib := range imageBuilds.Items {
		state := ib.Status.Phase
		if state == hephv1.PhaseFailed || state == hephv1.PhaseSucceeded {
			builds = append(builds, ib)
		}
	}

	if len(builds) <= gc.HistoryLimit {
		return nil
	}

	sort.Slice(builds, func(i, j int) bool {
		iTS := builds[i].CreationTimestamp
		jTS := builds[j].CreationTimestamp
		if !iTS.Equal(&jTS) {
			return iTS.Before(&jTS)
		}
		return builds[i].Name < builds[j].Name
	})

	builds = builds[:len(builds)-gc.HistoryLimit]
	logger.Info("Deleting ImageBuilds", "imageBuildsToRemove", len(builds))

	var errList []error
	for i := range builds {
		if err := ctx.Err(); err != nil {
			errList = append(errList, err)
			break
		}
		build := builds[i]
		if err := gc.Client.Delete(ctx, &build, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
			logger.Error(err, "Failed to delete image build", "imageBuild", build.Name, "namespace", build.Namespace)
			errList = append(errList, err)
		} else {
			logger.Info("Deleted image build", "imageBuild", build.Name, "namespace", build.Namespace)
		}
	}

	return errors.Join(errList...)
}
