// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"

	v1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
)

// FakeImageBuilds implements ImageBuildInterface
type FakeImageBuilds struct {
	Fake *FakeHephaestusV1
	ns   string
}

var imagebuildsResource = v1.SchemeGroupVersion.WithResource("imagebuilds")

var imagebuildsKind = v1.SchemeGroupVersion.WithKind("ImageBuild")

// Get takes name of the imageBuild, and returns the corresponding imageBuild object, and an error if there is any.
func (c *FakeImageBuilds) Get(ctx context.Context, name string, options metav1.GetOptions) (result *v1.ImageBuild, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewGetAction(imagebuildsResource, c.ns, name), &v1.ImageBuild{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1.ImageBuild), err
}

// List takes label and field selectors, and returns the list of ImageBuilds that match those selectors.
func (c *FakeImageBuilds) List(ctx context.Context, opts metav1.ListOptions) (result *v1.ImageBuildList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewListAction(imagebuildsResource, imagebuildsKind, c.ns, opts), &v1.ImageBuildList{})

	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1.ImageBuildList{ListMeta: obj.(*v1.ImageBuildList).ListMeta}
	for _, item := range obj.(*v1.ImageBuildList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested imageBuilds.
func (c *FakeImageBuilds) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewWatchAction(imagebuildsResource, c.ns, opts))

}

// Create takes the representation of a imageBuild and creates it.  Returns the server's representation of the imageBuild, and an error, if there is any.
func (c *FakeImageBuilds) Create(ctx context.Context, imageBuild *v1.ImageBuild, opts metav1.CreateOptions) (result *v1.ImageBuild, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewCreateAction(imagebuildsResource, c.ns, imageBuild), &v1.ImageBuild{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1.ImageBuild), err
}

// Update takes the representation of a imageBuild and updates it. Returns the server's representation of the imageBuild, and an error, if there is any.
func (c *FakeImageBuilds) Update(ctx context.Context, imageBuild *v1.ImageBuild, opts metav1.UpdateOptions) (result *v1.ImageBuild, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateAction(imagebuildsResource, c.ns, imageBuild), &v1.ImageBuild{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1.ImageBuild), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeImageBuilds) UpdateStatus(ctx context.Context, imageBuild *v1.ImageBuild, opts metav1.UpdateOptions) (*v1.ImageBuild, error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateSubresourceAction(imagebuildsResource, "status", c.ns, imageBuild), &v1.ImageBuild{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1.ImageBuild), err
}

// Delete takes name of the imageBuild and deletes it. Returns an error if one occurs.
func (c *FakeImageBuilds) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewDeleteActionWithOptions(imagebuildsResource, c.ns, name, opts), &v1.ImageBuild{})

	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeImageBuilds) DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error {
	action := testing.NewDeleteCollectionAction(imagebuildsResource, c.ns, listOpts)

	_, err := c.Fake.Invokes(action, &v1.ImageBuildList{})
	return err
}

// Patch applies the patch and returns the patched imageBuild.
func (c *FakeImageBuilds) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *v1.ImageBuild, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceAction(imagebuildsResource, c.ns, name, pt, data, subresources...), &v1.ImageBuild{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1.ImageBuild), err
}
