package kubehelpers

import (
	"context"
	"errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"strings"
	"sync"
)

type DockerImage struct {
	Registry *string
	Name     string
	Tag      string
}

type imageSpec struct {
	Image   string
	IsValid bool
}

func appendImagesInUseInSpec(specSource string, imagesInUse *[]imageSpec, podSpec corev1.PodSpec) {
	for _, container := range append(podSpec.Containers, podSpec.InitContainers...) {
		isValid := len(container.Image) > 0
		if !isValid {
			println("Found invalid image in container " + container.Name + " of " + specSource + ": " + container.Image)
		}
		*imagesInUse = append(*imagesInUse, imageSpec{container.Image, isValid})
	}
	for _, container := range podSpec.EphemeralContainers {
		isValid := len(container.Image) > 0
		if !isValid {
			println("Found invalid image in ephemeral container " + container.Name + " of " + specSource + ": " + container.Image)
		}
		*imagesInUse = append(*imagesInUse, imageSpec{container.Image, isValid})
	}
}

func imagesInPods(ctx context.Context, clients *kubernetes.Clientset, namespace string) ([]imageSpec, error) {
	list, err := clients.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	imagesInUse := make([]imageSpec, 0, len(list.Items))
	for _, pod := range list.Items {
		for _, status := range append(pod.Status.ContainerStatuses, append(pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses...)...) {
			if status.Ready || (status.Started != nil && *status.Started) || status.State.Running != nil || status.State.Waiting != nil {
				if len(status.ImageID) == 0 {
					println("Found invalid (empty) image in pod " + pod.Name + " with status " + status.State.String() + ": " + status.ImageID + ". Checking spec...")
					appendImagesInUseInSpec("pod "+pod.Name+" ", &imagesInUse, pod.Spec)
				} else {
					imagesInUse = append(imagesInUse, imageSpec{status.ImageID, true})
				}
			}
		}
	}
	return imagesInUse, nil
}

func imagesInCronJobs(ctx context.Context, clients *kubernetes.Clientset, namespace string) ([]imageSpec, error) {
	list, err := clients.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	imagesInUse := make([]imageSpec, 0, len(list.Items))
	for _, job := range list.Items {
		appendImagesInUseInSpec("cron job "+job.Name+" ", &imagesInUse, job.Spec.JobTemplate.Spec.Template.Spec)
	}
	return imagesInUse, nil
}

func imagesInJobs(ctx context.Context, clients *kubernetes.Clientset, namespace string) ([]imageSpec, error) {
	list, err := clients.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	imagesInUse := make([]imageSpec, 0, len(list.Items))
	for _, job := range list.Items {
		appendImagesInUseInSpec("job "+job.Name+" ", &imagesInUse, job.Spec.Template.Spec)
	}
	return imagesInUse, nil
}

func imagesInDeployments(ctx context.Context, clients *kubernetes.Clientset, namespace string) ([]imageSpec, error) {
	list, err := clients.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	imagesInUse := make([]imageSpec, 0, len(list.Items))
	for _, deployment := range list.Items {
		appendImagesInUseInSpec("deployment "+deployment.Name+" ", &imagesInUse, deployment.Spec.Template.Spec)
	}
	return imagesInUse, nil
}

func imagesInStatefulSets(ctx context.Context, clients *kubernetes.Clientset, namespace string) ([]imageSpec, error) {
	list, err := clients.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	imagesInUse := make([]imageSpec, 0, len(list.Items))
	for _, statefulSet := range list.Items {
		appendImagesInUseInSpec("stateful set "+statefulSet.Name+" ", &imagesInUse, statefulSet.Spec.Template.Spec)
	}
	return imagesInUse, nil
}

func FindImagesInUse(ctx context.Context, config Config) ([]DockerImage, error) {
	kubeConfig, err := clientcmd.BuildConfigFromFlags("", config.KubeConfigPath)
	if err != nil {
		return nil, err
	}

	clients, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}

	outputChannel := make(chan imageSpec)
	errChannel := make(chan error, 1)
	sendError := func(errorChannel chan<- error, err error) {
		select {
		case errorChannel <- err:
		default:
			break
		}
	}
	var wg sync.WaitGroup
	runJob := func(job func() ([]imageSpec, error)) {
		wg.Add(1)
		go func(errorChannel chan<- error) {
			defer wg.Done()
			images, err := job()
			if err != nil {
				sendError(errChannel, err)
			} else {
				for _, image := range images {
					outputChannel <- image
				}
			}
		}(errChannel)
	}
	runJob(func() ([]imageSpec, error) {
		return imagesInPods(ctx, clients, config.Namespace)
	})
	runJob(func() ([]imageSpec, error) {
		return imagesInCronJobs(ctx, clients, config.Namespace)
	})
	runJob(func() ([]imageSpec, error) {
		return imagesInJobs(ctx, clients, config.Namespace)
	})
	runJob(func() ([]imageSpec, error) {
		return imagesInDeployments(ctx, clients, config.Namespace)
	})
	runJob(func() ([]imageSpec, error) {
		return imagesInStatefulSets(ctx, clients, config.Namespace)
	})
	go func() {
		wg.Wait()
		close(outputChannel)
	}()

	defer close(errChannel)

	processedImageStrings := make(map[string]bool)
	imagesInUse := make([]DockerImage, 0)
	invalidImages := make([]string, 0)
	for imageSpec := range outputChannel {
		if _, found := processedImageStrings[imageSpec.Image]; found {
			continue
		}
		if !imageSpec.IsValid {
			println("Found invalid image: " + imageSpec.Image)
			invalidImages = append(invalidImages, imageSpec.Image)
			continue
		}
		imageParts := strings.SplitN(imageSpec.Image, ":", 2)
		if len(imageParts) != 2 {
			println("Found invalid image (after splitting): " + imageSpec.Image)
			invalidImages = append(invalidImages, imageSpec.Image)
			continue
		}
		var registry *string
		var name string
		if strings.Contains(imageParts[0], ".") {
			registryParts := strings.SplitN(imageParts[0], "/", 2)
			registry = &registryParts[0]
			name = registryParts[1]
		} else {
			registry = nil
			name = imageParts[0]
		}
		imagesInUse = append(imagesInUse, DockerImage{
			Registry: registry,
			Name:     name,
			Tag:      imageParts[1],
		})
		processedImageStrings[imageSpec.Image] = true
	}

	if len(errChannel) > 0 {
		return nil, <-errChannel
	}

	if len(invalidImages) > 0 { // We cannot skip these, or it might delete images that are in use.
		return nil, errors.New("Invalid images found: " + strings.Join(invalidImages, ", "))
	}

	return imagesInUse, nil
}
