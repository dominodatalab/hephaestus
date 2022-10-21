package functional

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/clientset"
	"github.com/dominodatalab/hephaestus/pkg/messaging/amqp"
	"github.com/go-playground/validator/v10"
	"github.com/go-redis/redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

var seededRand = rand.New(rand.NewSource(time.Now().UnixNano()))

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomString(length int) string {
	bs := make([]byte, length)
	for i := range bs {
		bs[i] = charset[seededRand.Intn(len(charset))]
	}
	return string(bs)
}

type remoteDockerBuildContext int

const (
	contextServer = "https://raw.githubusercontent.com/dominodatalab/hephaestus/complete-gke-testing/test/functional/testdata/docker-context/%s/archive.tgz"

	buildArgBuildContext remoteDockerBuildContext = iota
	errorBuildContext
	python39JupyterBuildContext
	multiStageBuildContext
)

func (c remoteDockerBuildContext) String() string {
	return [...]string{
		"build-arg",
		"error",
		"python39-jupyter",
		"multi-stage",
	}[c-1]
}

func newImageBuild(bc remoteDockerBuildContext, image string, creds *hephv1.RegistryCredentials) *hephv1.ImageBuild {
	uid := randomString(8)
	dockerContextURL := fmt.Sprintf(contextServer, bc.String())

	var auth []hephv1.RegistryCredentials
	if creds != nil {
		auth = append(auth, *creds)
	}

	return &hephv1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-build-",
		},
		Spec: hephv1.ImageBuildSpec{
			Images:                  []string{fmt.Sprintf("%s:%s", image, uid)},
			LogKey:                  uid,
			Context:                 dockerContextURL,
			RegistryAuth:            auth,
			DisableCacheLayerExport: true,
		},
	}
}

func createBuild(t *testing.T, ctx context.Context, client clientset.Interface, build *hephv1.ImageBuild) *hephv1.ImageBuild {
	t.Helper()

	ibClient := client.HephaestusV1().ImageBuilds(corev1.NamespaceDefault)

	build, err := ibClient.Create(ctx, build, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create build")

	watcher, err := ibClient.Watch(ctx, metav1.SingleObject(build.ObjectMeta))
	require.NoError(t, err, "failed to create build watch")
	defer watcher.Stop()

	ctxTimeout, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var result *hephv1.ImageBuild
	for loop := true; loop; {
		select {
		case <-ctxTimeout.Done():
			t.Fatal("Build failed to finish within context deadline")
		case event := <-watcher.ResultChan():
			if event.Type != watch.Modified {
				continue
			}

			result = event.Object.(*hephv1.ImageBuild)
			if (result.Status.Phase == hephv1.PhaseSucceeded || result.Status.Phase == hephv1.PhaseFailed) && result.Status.Conditions != nil {
				loop = false
			}
		}
	}

	return result
}

type logEvent struct {
	Event    map[string]string `json:"event" validate:"required"`
	Stream   string            `json:"stream" validate:"required"`
	Time     time.Time         `json:"time" validate:"required"`
	TimeNano int64             `json:"time_nano,string" validate:"required"`
	Log      string            `json:"log" validate:"required"`
	LogKey   string            `json:"logKey" validate:"required"`
}

func testLogDelivery(t *testing.T, ctx context.Context, client kubernetes.Interface, build *hephv1.ImageBuild) {
	t.Helper()

	svc, err := client.CoreV1().Services(corev1.NamespaceDefault).Get(
		ctx,
		"redis-master",
		metav1.GetOptions{},
	)
	require.NoError(t, err)

	rdb := redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:6379", svc.Status.LoadBalancer.Ingress[0].IP),
		Password: "redis-password",
	})

	var logEvents []string
	err = wait.PollImmediateWithContext(
		ctx,
		1*time.Second,
		10*time.Second,
		func(ctx context.Context) (done bool, err error) {
			logEvents, err = rdb.LRange(ctx, build.Spec.LogKey, 0, -1).Result()
			return len(logEvents) != 0, err
		},
	)
	require.NoError(t, err, "unable to find any redis log events")

	validate := validator.New()
	for _, event := range logEvents {
		var data logEvent

		require.NoError(t, json.Unmarshal([]byte(event), &data))
		require.NoError(t, validate.Struct(data))
	}
}

func testMessageDelivery(t *testing.T, ctx context.Context, client kubernetes.Interface, build *hephv1.ImageBuild) {
	t.Helper()

	svc, err := client.CoreV1().Services(corev1.NamespaceDefault).Get(
		ctx,
		"rabbitmq",
		metav1.GetOptions{},
	)
	require.NoError(t, err, "failed to get rabbitmq service")

	rmqURL := fmt.Sprintf("amqp://user:rabbitmq-password@%s:5672/", svc.Status.LoadBalancer.Ingress[0].IP)
	conn, channel, err := amqp.Dial(rmqURL)
	require.NoError(t, err, "failed to connet to rabbitmq service")
	defer func() {
		channel.Close()
		conn.Close()
	}()

	queue, err := channel.QueueInspect("hephaestus.imagebuilds.status")
	require.NoError(t, err)

	deliveryCh, err := channel.Consume("hephaestus.imagebuilds.status", "", true, false, false, false, nil)
	require.NoError(t, err)

	var messages []hephv1.ImageBuildStatusTransitionMessage
	for i := 0; i < queue.Messages; i++ {
		select {
		case message := <-deliveryCh:
			var data hephv1.ImageBuildStatusTransitionMessage
			require.NoError(t, json.Unmarshal(message.Body, &data))

			if data.Name == build.Name {
				messages = append(messages, data)
			}
		}
	}
	require.Len(t, messages, 3)

	assert.Equal(t, messages[0].CurrentPhase, hephv1.PhaseInitializing)
	assert.Equal(t, messages[1].CurrentPhase, hephv1.PhaseRunning)

	finalTransition := messages[2]
	assert.Equal(t, finalTransition.CurrentPhase, build.Status.Phase)

	if build.Status.Phase == hephv1.PhaseSucceeded {
		assert.Equal(t, build.Spec.Images, finalTransition.ImageURLs)
	} else {
		assert.NotEmpty(t, finalTransition.ErrorMessage)
	}
}
