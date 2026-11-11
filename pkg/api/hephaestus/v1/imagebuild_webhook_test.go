package v1

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// admission.WithDefaulter[*ImageBuild] binds Default's method receiver to a single static template
// registered once at manager startup and always calls it as template.Default(ctx, obj), where obj is
// the freshly-decoded per-request object - a distinct value from the receiver. A Default() that
// mutates its receiver instead of obj silently does nothing in production, since only obj's
// mutations reach the admission response. This test calls Default the same way to catch that
// regression, instead of obj.Default(ctx, obj) where receiver and param are the same value and a
// receiver-mutation bug is invisible.
func TestImageBuildDefault(t *testing.T) {
	t.Run("dedupes platforms case-insensitively", func(t *testing.T) {
		template := &ImageBuild{}
		obj := &ImageBuild{Spec: ImageBuildSpec{
			Platforms: []string{"linux/amd64", "LINUX/AMD64", "linux/arm64"},
		}}

		assert.NoError(t, template.Default(context.Background(), obj))
		assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, obj.Spec.Platforms)
		assert.Nil(t, template.Spec.Platforms, "Default must not mutate its receiver")
	})

	t.Run("nil platforms stay nil", func(t *testing.T) {
		template := &ImageBuild{}
		obj := &ImageBuild{}

		assert.NoError(t, template.Default(context.Background(), obj))
		assert.Nil(t, obj.Spec.Platforms)
	})
}

func TestImageBuildValidateAdditionalContexts(t *testing.T) {
	sum := strings.Repeat("a", 64)
	valid := func() AdditionalContext {
		return AdditionalContext{
			Name:         "data_1",
			URL:          "https://example.com/ctx.tar.gz",
			SHA256:       sum,
			TokenSecret:  &SecretKeyReference{Name: "tok", Key: "token"},
			MaxSizeBytes: 1024,
		}
	}
	build := func(acs ...AdditionalContext) *ImageBuild {
		return &ImageBuild{Spec: ImageBuildSpec{
			DockerfileContents: "FROM scratch",
			Images:             []string{"example.com/foo:bar"},
			AdditionalContexts: acs,
		}}
	}
	mod := func(f func(*AdditionalContext)) AdditionalContext {
		ac := valid()
		f(&ac)

		return ac
	}
	named := func(name string) AdditionalContext {
		return mod(func(a *AdditionalContext) { a.Name = name })
	}

	const (
		nameField  = "spec.additionalContexts[0].name"
		urlField   = "spec.additionalContexts[0].url"
		shaField   = "spec.additionalContexts[0].sha256"
		sizeField  = "spec.additionalContexts[0].maxSizeBytes"
		tokenName  = "spec.additionalContexts[0].tokenSecret.name"
		tokenKey   = "spec.additionalContexts[0].tokenSecret.key"
		secondName = "spec.additionalContexts[1].name"
	)

	// invalidFields returns the field paths reported by an admission rejection.
	invalidFields := func(t *testing.T, err error) []string {
		t.Helper()

		var statusErr *apierrors.StatusError
		require.True(t, errors.As(err, &statusErr), "expected a StatusError, got %T: %v", err, err)
		require.True(t, apierrors.IsInvalid(err), "expected an Invalid status, got: %v", err)
		require.NotNil(t, statusErr.ErrStatus.Details)

		var fields []string
		for _, cause := range statusErr.ErrStatus.Details.Causes {
			fields = append(fields, cause.Field)
		}

		return fields
	}

	template := &ImageBuild{}

	t.Run("valid", func(t *testing.T) {
		cases := map[string][]AdditionalContext{
			"none":                  nil,
			"https with token":      {valid()},
			"http without token":    {mod(func(a *AdditionalContext) { a.URL, a.TokenSecret = "http://example.com/x", nil })},
			"https without token":   {mod(func(a *AdditionalContext) { a.TokenSecret = nil })},
			"multiple distinct":     {named("one"), named("two")},
			"dotted name":           {named("my.data")},
			"hyphenated name":       {named("my-data")},
			"double underscore":     {named("my__data")},
			"multiple hyphens":      {named("my--data")},
			"single letter":         {named("a")},
			"max length name":       {named("a" + strings.Repeat("b", 62))},
			"smallest max size":     {mod(func(a *AdditionalContext) { a.MaxSizeBytes = 1 })},
			"digits after a letter": {named("a1b2")},
		}
		for name, acs := range cases {
			t.Run(name, func(t *testing.T) {
				_, err := template.ValidateCreate(context.Background(), build(acs...))
				assert.NoError(t, err)
			})
		}
	})

	invalid := map[string]struct {
		contexts []AdditionalContext
		fields   []string
	}{
		"empty name":            {[]AdditionalContext{named("")}, []string{nameField}},
		"blank name":            {[]AdditionalContext{named("   ")}, []string{nameField}},
		"duplicate name":        {[]AdditionalContext{valid(), valid()}, []string{secondName}},
		"reserved context":      {[]AdditionalContext{named("context")}, []string{nameField}},
		"reserved dockerfile":   {[]AdditionalContext{named("dockerfile")}, []string{nameField}},
		"uppercase name":        {[]AdditionalContext{named("Data")}, []string{nameField}},
		"mixed case name":       {[]AdditionalContext{named("myData")}, []string{nameField}},
		"trailing hyphen":       {[]AdditionalContext{named("data-")}, []string{nameField}},
		"trailing underscore":   {[]AdditionalContext{named("data_")}, []string{nameField}},
		"trailing dot":          {[]AdditionalContext{named("data.")}, []string{nameField}},
		"leading hyphen":        {[]AdditionalContext{named("-data")}, []string{nameField}},
		"adjacent separators":   {[]AdditionalContext{named("da-_ta")}, []string{nameField}},
		"triple underscore":     {[]AdditionalContext{named("da___ta")}, []string{nameField}},
		"name too long":         {[]AdditionalContext{named("a" + strings.Repeat("b", 63))}, []string{nameField}},
		"name with space":       {[]AdditionalContext{named("my ctx")}, []string{nameField}},
		"name leading digit":    {[]AdditionalContext{named("1abc")}, []string{nameField}},
		"name leading symbol":   {[]AdditionalContext{named("_abc")}, []string{nameField}},
		"name with slash":       {[]AdditionalContext{named("a/b")}, []string{nameField}},
		"name with colon":       {[]AdditionalContext{named("a:b")}, []string{nameField}},
		"empty url":             {[]AdditionalContext{mod(func(a *AdditionalContext) { a.URL = "" })}, []string{urlField}},
		"ftp url":               {[]AdditionalContext{mod(func(a *AdditionalContext) { a.URL = "ftp://example.com/x" })}, []string{urlField}},
		"file url":              {[]AdditionalContext{mod(func(a *AdditionalContext) { a.URL = "file:///etc/passwd" })}, []string{urlField}},
		"schemeless url":        {[]AdditionalContext{mod(func(a *AdditionalContext) { a.URL = "example.com/x" })}, []string{urlField}},
		"http url with token":   {[]AdditionalContext{mod(func(a *AdditionalContext) { a.URL = "http://example.com/x" })}, []string{urlField}},
		"missing sha256":        {[]AdditionalContext{mod(func(a *AdditionalContext) { a.SHA256 = "" })}, []string{shaField}},
		"short sha":             {[]AdditionalContext{mod(func(a *AdditionalContext) { a.SHA256 = "abc" })}, []string{shaField}},
		"uppercase sha":         {[]AdditionalContext{mod(func(a *AdditionalContext) { a.SHA256 = strings.ToUpper(sum) })}, []string{shaField}},
		"non-hex sha":           {[]AdditionalContext{mod(func(a *AdditionalContext) { a.SHA256 = strings.Repeat("g", 64) })}, []string{shaField}},
		"missing max size":      {[]AdditionalContext{mod(func(a *AdditionalContext) { a.MaxSizeBytes = 0 })}, []string{sizeField}},
		"negative max size":     {[]AdditionalContext{mod(func(a *AdditionalContext) { a.MaxSizeBytes = -1 })}, []string{sizeField}},
		"token missing name":    {[]AdditionalContext{mod(func(a *AdditionalContext) { a.TokenSecret = &SecretKeyReference{Key: "token"} })}, []string{tokenName}},
		"token missing key":     {[]AdditionalContext{mod(func(a *AdditionalContext) { a.TokenSecret = &SecretKeyReference{Name: "tok"} })}, []string{tokenKey}},
		"token blank name":      {[]AdditionalContext{mod(func(a *AdditionalContext) { a.TokenSecret = &SecretKeyReference{Name: " ", Key: "token"} })}, []string{tokenName}},
		"token empty reference": {[]AdditionalContext{mod(func(a *AdditionalContext) { a.TokenSecret = &SecretKeyReference{} })}, []string{tokenName, tokenKey}},
		"every field invalid": {
			[]AdditionalContext{{Name: "Bad", URL: "ftp://x", SHA256: "x", MaxSizeBytes: 0}},
			[]string{nameField, urlField, shaField, sizeField},
		},
	}
	for name, tc := range invalid {
		t.Run(name, func(t *testing.T) {
			_, err := template.ValidateCreate(context.Background(), build(tc.contexts...))
			require.Error(t, err)
			assert.ElementsMatch(t, tc.fields, invalidFields(t, err))
		})
	}

	t.Run("update is validated too", func(t *testing.T) {
		_, err := template.ValidateUpdate(context.Background(), build(valid()), build(mod(func(a *AdditionalContext) {
			a.SHA256 = ""
		})))
		require.Error(t, err)
		assert.ElementsMatch(t, []string{shaField}, invalidFields(t, err))
	})
}

// The token Secret is always read from the ImageBuild's own namespace, so a cross-namespace reference must not be
// expressible through the API.
func TestSecretKeyReferenceHasNoNamespace(t *testing.T) {
	_, found := reflect.TypeOf(SecretKeyReference{}).FieldByName("Namespace")
	assert.False(t, found, "SecretKeyReference must not expose a Namespace")

	var ref SecretKeyReference
	require.NoError(t, json.Unmarshal([]byte(`{"name":"tok","key":"token","namespace":"other"}`), &ref))
	assert.Equal(t, SecretKeyReference{Name: "tok", Key: "token"}, ref)

	out, err := json.Marshal(ref)
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"tok","key":"token"}`, string(out))
}
