package component

import (
	"context"
	"errors"
	"sort"
	"time"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var ErrMissingNamespaces = errors.New("no namespaces specified")

type ImageBuildGC struct {
	HistoryLimit int
	Client       client.Client
	Namespaces   []string
}

func (gc *ImageBuildGC) Start(ctx context.Context) error {
	if len(gc.Namespaces) == 0 {
		return ErrMissingNamespaces
	}

	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithName("controller").WithName("imagebuild").WithName("gc"))

	ticker := time.NewTicker(time.Hour)
	for {
		_ = gc.GC(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (gc *ImageBuildGC) GC(ctx context.Context) error {
	var errs []error
	for i := range gc.Namespaces {
		errs = append(errs, gc.gc(ctx, gc.Namespaces[i]))
	}
	return errors.Join(errs...)
}

func (gc *ImageBuildGC) gc(ctx context.Context, namespace string) error {
	logger := log.FromContext(ctx)
	if namespace != "" {
		logger = logger.WithValues("namespace", namespace)
	}
	logger.Info("Image Build GC starting")
	defer logger.Info("Image Build GC finished")

	imageBuilds := &hephv1.ImageBuildList{}
	err := gc.Client.List(ctx, imageBuilds, client.InNamespace(namespace))
	if err != nil {
		logger.Error(err, "ImageBuilds.List failed")
		return err
	}

	listLen := len(imageBuilds.Items)
	if listLen == 0 {
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

	logger.Info("Removing ImageBuilds", "count", len(builds))
	sort.Slice(builds, func(i, j int) bool {
		iTS := builds[i].CreationTimestamp
		jTS := builds[j].CreationTimestamp
		if before := iTS.Before(&jTS); before {
			return true
		}
		if iTS.Equal(&jTS) {
			if builds[i].Namespace < builds[j].Namespace {
				return true
			}
			return builds[i].Name < builds[j].Name
		}
		return false
	})
	builds = builds[:len(builds)-gc.HistoryLimit]

	var errList []error
	for i := range builds {
		build := builds[i]
		if err := gc.Client.Delete(ctx, &build, client.PropagationPolicy(metav1.DeletePropagationForeground)); err == nil {
			logger.Info("Deleted image build", "imageBuild", build.Name, "namespace", build.Namespace)
		} else {
			logger.Error(err, "Failed to delete image build", "imageBuild", build.Name, "namespace", build.Namespace)
			errList = append(errList, err)
		}
	}

	return errors.Join(errList...)
}
